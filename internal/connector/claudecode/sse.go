package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/blestafist/pestiroute/internal/core"
)

const maxSSEBytes = 1 << 20
const maxRetainedResponseBytes = 1 << 20

type sseEvent struct {
	name string
	data []byte
}
type sseReader struct {
	r             io.ReadCloser
	buf           [4096]byte
	pos, n        int
	err           error
	line, data    []byte
	event         string
	hasData, cr   bool
	eofDispatched bool
}

func newSSEReader(r io.ReadCloser) *sseReader { return &sseReader{r: r} }
func (s *sseReader) next(ctx context.Context) (sseEvent, error) {
	for {
		if err := ctx.Err(); err != nil {
			return sseEvent{}, err
		}
		if s.pos == s.n {
			if s.err != nil {
				if s.err != io.EOF {
					return sseEvent{}, s.err
				}
				if !s.eofDispatched && len(s.line) == 0 && s.hasData {
					s.eofDispatched = true
					return s.dispatch()
				}
				return sseEvent{}, io.EOF
			}
			s.n, s.err = s.r.Read(s.buf[:])
			s.pos = 0
			if s.n == 0 && s.err == nil {
				return sseEvent{}, io.ErrNoProgress
			}
			continue
		}
		b := s.buf[s.pos]
		s.pos++
		if s.cr {
			s.cr = false
			if b == '\n' {
				continue
			}
		}
		if b == '\r' || b == '\n' {
			blank := len(s.line) == 0
			if err := s.lineDone(); err != nil {
				return sseEvent{}, err
			}
			if b == '\r' {
				s.cr = true
			}
			if blank && s.hasData {
				e, err := s.dispatch()
				if err != nil {
					return sseEvent{}, err
				}
				return e, nil
			}
			continue
		}
		if len(s.line) >= maxSSEBytes {
			return sseEvent{}, errors.New("SSE line exceeds 1 MiB")
		}
		s.line = append(s.line, b)
	}
}
func (s *sseReader) lineDone() error {
	line := s.line
	s.line = nil
	if len(line) == 0 || line[0] == ':' {
		return nil
	}
	field, value, found := bytes.Cut(line, []byte{':'})
	if !found {
		field, value = line, nil
	}
	if len(value) > 0 && value[0] == ' ' {
		value = value[1:]
	}
	switch string(field) {
	case "event":
		if !utf8.Valid(value) {
			return errors.New("invalid UTF-8 event name")
		}
		s.event = string(value)
	case "data":
		if len(s.data)+len(value)+1 > maxSSEBytes {
			return errors.New("SSE event exceeds 1 MiB")
		}
		if s.hasData {
			s.data = append(s.data, '\n')
		}
		s.data = append(s.data, value...)
		s.hasData = true
	case "id", "retry":
		return errors.New("unsupported SSE field")
	default:
		return errors.New("unknown SSE field")
	}
	return nil
}
func (s *sseReader) dispatch() (sseEvent, error) {
	defer func() { s.event = ""; s.data = nil; s.hasData = false }()
	if !utf8.Valid(s.data) {
		return sseEvent{}, errors.New("invalid UTF-8 SSE data")
	}
	var value struct {
		Type string `json:"type"`
	}
	if !uniqueJSON(s.data) || !json.Valid(s.data) || json.Unmarshal(s.data, &value) != nil {
		return sseEvent{}, errors.New("invalid SSE JSON")
	}
	name := s.event
	if name == "" {
		name = value.Type
	}
	if name != value.Type {
		return sseEvent{}, errors.New("SSE event type mismatch")
	}
	switch name {
	case "message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop", "error":
	default:
		return sseEvent{}, fmt.Errorf("unknown Messages event %q", name)
	}
	return sseEvent{name: name, data: bytes.Clone(s.data)}, nil
}

type messagesStream struct {
	r                                 *sseReader
	mu                                sync.Mutex
	closed                            bool
	closeOnce                         sync.Once
	closeErr                          error
	phase                             uint8
	started, block, tool, stopped     bool
	index, next                       int
	blockType, stopReason, responseID string
	itemID                            string
	input, output                     *int64
	pending                           [][]byte
	terminal                          *core.StreamFrame
	sequence                          int64
	// Retain only the bounded final Responses projection while streaming delta events immediately.
	outputs         []map[string]any
	item            map[string]any
	text, arguments strings.Builder
	retained        int
}

// newMessagesStream takes ownership of r; Close must unblock an active Read.
func newMessagesStream(r io.ReadCloser) *messagesStream {
	return &messagesStream{r: newSSEReader(r), outputs: make([]map[string]any, 0)}
}
func (s *messagesStream) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		s.closeErr = s.r.r.Close()
	})
	return s.closeErr
}

func (s *messagesStream) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *messagesStream) Next(ctx context.Context) (core.StreamFrame, error) {
	if err := ctx.Err(); err != nil {
		_ = s.Close()
		return core.StreamFrame{}, err
	}
	stop := context.AfterFunc(ctx, func() { _ = s.Close() })
	defer stop()
	if s.phase == 3 {
		return core.StreamFrame{}, io.EOF
	}
	if s.isClosed() {
		return core.StreamFrame{}, io.EOF
	}
	if s.phase == 0 {
		s.phase = 1
		e, err := s.r.next(ctx)
		if ctx.Err() != nil {
			_ = s.Close()
			return core.StreamFrame{}, ctx.Err()
		}
		if s.isClosed() {
			return core.StreamFrame{}, context.Canceled
		}
		if err == nil && e.name == "message_start" {
			err = s.consume(e)
		}
		if err != nil || e.name != "message_start" {
			failure := &core.GatewayError{Code: "invalid_response", Category: core.CategoryUnavailable, Message: "Upstream response did not start a valid message"}
			outcome := core.OutcomeFailed
			if err == io.EOF {
				failure.Code = "incomplete_response"
				failure.Message = "Upstream response ended before message_start"
			} else if err != nil {
				failure, outcome = readFailure(err)
			}
			s.terminal = &core.StreamFrame{Type: core.FrameComplete, Complete: &core.CompleteFrame{Outcome: outcome, Error: failure, Usage: s.usage(core.UsagePartial)}}
			return core.StreamFrame{Type: core.FrameHead, Head: &core.HeadFrame{Protocol: protocol, ContentType: "text/event-stream", Error: failure}}, nil
		}
		return core.StreamFrame{Type: core.FrameHead, Head: &core.HeadFrame{Protocol: protocol, ContentType: "text/event-stream"}}, nil
	}
	if len(s.pending) > 0 {
		return body(s.pop()), nil
	}
	if s.terminal != nil {
		f := *s.terminal
		s.terminal = nil
		s.phase = 3
		_ = s.Close()
		return f, nil
	}
	for {
		e, err := s.r.next(ctx)
		if ctx.Err() != nil {
			_ = s.Close()
			return core.StreamFrame{}, ctx.Err()
		}
		if s.isClosed() {
			return core.StreamFrame{}, context.Canceled
		}
		if err != nil {
			code, msg := "incomplete_response", "Upstream response ended before message_stop"
			category := core.CategoryUnavailable
			outcome := core.OutcomeIncomplete
			if err != io.EOF {
				failure, failureOutcome := readFailure(err)
				return s.fail(failure.Code, failure.Message, failure.Category, failureOutcome), nil
			}
			return s.fail(code, msg, category, outcome), nil
		}
		if err = s.consume(e); err != nil {
			return s.fail("invalid_response", "Upstream response was invalid", core.CategoryUnavailable, core.OutcomeFailed), nil
		}
		if len(s.pending) > 0 {
			return body(s.pop()), nil
		}
		if s.terminal != nil {
			f := *s.terminal
			s.terminal = nil
			s.phase = 3
			_ = s.Close()
			return f, nil
		}
	}
}
func (s *messagesStream) pop() []byte { b := s.pending[0]; s.pending = s.pending[1:]; return b }
func body(b []byte) core.StreamFrame {
	return core.StreamFrame{Type: core.FrameBody, Body: &core.BodyFrame{Data: b}}
}
func (s *messagesStream) fail(code, msg string, category core.ErrorCategory, outcome core.Outcome) core.StreamFrame {
	s.phase = 3
	_ = s.Close()
	return core.StreamFrame{Type: core.FrameComplete, Complete: &core.CompleteFrame{Outcome: outcome, Error: &core.GatewayError{Code: code, Category: category, Message: msg}, Usage: s.usage(core.UsagePartial)}}
}

func readFailure(err error) (*core.GatewayError, core.Outcome) {
	switch {
	case errors.Is(err, context.Canceled):
		return &core.GatewayError{Code: "upstream_cancelled", Category: core.CategoryCancelled, Message: "Upstream stream was cancelled"}, core.OutcomeCancelled
	case errors.Is(err, context.DeadlineExceeded):
		return &core.GatewayError{Code: "upstream_timeout", Category: core.CategoryTimeout, Message: "Upstream stream timed out"}, core.OutcomeIncomplete
	default:
		return &core.GatewayError{Code: "upstream_read_error", Category: core.CategoryUnavailable, Message: "Upstream response could not be read"}, core.OutcomeFailed
	}
}
func (s *messagesStream) usage(c core.UsageCompleteness) *core.UsageReport {
	source := core.UsageProvider
	if s.input == nil && s.output == nil {
		source = core.UsageUnknown
	}
	return &core.UsageReport{InputTokens: s.input, OutputTokens: s.output, Source: source, Completeness: c}
}
func (s *messagesStream) consume(e sseEvent) error {
	switch e.name {
	case "error":
		return errors.New("upstream stream error")
	case "message_start":
		if s.started {
			return errors.New("duplicate message_start")
		}
		var v struct {
			Message struct {
				ID      string            `json:"id"`
				Content []json.RawMessage `json:"content"`
				Usage   struct {
					Input *int64 `json:"input_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(e.data, &v) != nil || v.Message.ID == "" || len(v.Message.ID) > 256 || len(v.Message.Content) != 0 || badCount(v.Message.Usage.Input) {
			return errors.New("bad message_start")
		}
		s.started = true
		s.responseID = v.Message.ID
		s.input = v.Message.Usage.Input
		s.pending = append(s.pending, s.emit("response.created", map[string]any{"response": map[string]any{"id": s.responseIDValue(), "object": "response", "status": "in_progress", "output": []any{}}}))
	case "content_block_start":
		var v struct {
			Index *int `json:"index"`
			Block struct {
				Type     string          `json:"type"`
				ID       string          `json:"id"`
				Name     string          `json:"name"`
				Text     *string         `json:"text"`
				Thinking string          `json:"thinking"`
				Input    json.RawMessage `json:"input"`
			} `json:"content_block"`
		}
		if !s.started || s.block || s.stopped || json.Unmarshal(e.data, &v) != nil || v.Index == nil || *v.Index != s.next || *v.Index >= 64 {
			return errors.New("bad block start")
		}
		s.index = *v.Index
		s.blockType = v.Block.Type
		s.block = true
		s.tool = v.Block.Type == "tool_use"
		switch v.Block.Type {
		case "text":
			s.itemID = fmt.Sprintf("msg_%s_%d", s.responseID, s.index)
			s.text.Reset()
			s.item = map[string]any{"id": s.itemID, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{map[string]any{"type": "output_text", "text": "", "annotations": []any{}, "logprobs": []any{}}}}
			s.pending = append(s.pending, s.emit("response.output_item.added", map[string]any{"output_index": s.index, "item": s.item}))
			s.pending = append(s.pending, s.emit("response.content_part.added", map[string]any{"output_index": s.index, "item_id": s.itemID, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}, "logprobs": []any{}}}))
		case "thinking":
			s.itemID = fmt.Sprintf("rs_%s_%d", s.responseID, s.index)
			s.text.Reset()
			s.item = map[string]any{"id": s.itemID, "type": "reasoning", "status": "in_progress", "summary": []any{map[string]any{"type": "summary_text", "text": ""}}}
			s.pending = append(s.pending, s.emit("response.output_item.added", map[string]any{"output_index": s.index, "item": s.item}))
			s.pending = append(s.pending, s.emit("response.reasoning_summary_part.added", map[string]any{"output_index": s.index, "item_id": s.itemID, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": ""}}))
		case "tool_use":
			if v.Block.ID == "" || v.Block.Name == "" || len(v.Block.ID)+len(v.Block.Name) > maxRetainedResponseBytes-s.retained {
				return errors.New("bad tool block")
			}
			if len(v.Block.Input) > 0 {
				var initial map[string]json.RawMessage
				if json.Unmarshal(v.Block.Input, &initial) != nil || initial == nil || len(initial) != 0 {
					return errors.New("unsupported initial tool input")
				}
			}
			s.retained += len(v.Block.ID) + len(v.Block.Name)
			s.itemID = v.Block.ID
			s.arguments.Reset()
			s.item = map[string]any{"id": s.itemID, "type": "function_call", "call_id": v.Block.ID, "name": v.Block.Name, "arguments": "", "status": "in_progress"}
			s.pending = append(s.pending, s.emit("response.output_item.added", map[string]any{"output_index": s.index, "item": s.item}))
		default:
			return errors.New("unsupported block")
		}
		initialDelta := ""
		if v.Block.Type == "text" && v.Block.Text != nil {
			initialDelta = *v.Block.Text
		}
		if v.Block.Type == "thinking" {
			initialDelta = v.Block.Thinking
		}
		if initialDelta != "" {
			if len(initialDelta) > maxRetainedResponseBytes-s.retained {
				return errors.New("Messages response exceeds bounded retention limit")
			}
			s.retained += len(initialDelta)
			s.text.WriteString(initialDelta)
			deltaEvent := "response.output_text.delta"
			deltaPayload := map[string]any{"output_index": s.index, "item_id": s.itemID, "delta": initialDelta}
			if v.Block.Type == "thinking" {
				deltaEvent = "response.reasoning_summary_text.delta"
				deltaPayload["summary_index"] = 0
			} else {
				deltaPayload["content_index"] = 0
			}
			s.pending = append(s.pending, s.emit(deltaEvent, deltaPayload))
		}
	case "content_block_delta":
		var v struct {
			Index *int `json:"index"`
			Delta struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				Thinking string `json:"thinking"`
				JSON     string `json:"partial_json"`
			} `json:"delta"`
		}
		if !s.block || json.Unmarshal(e.data, &v) != nil || v.Index == nil || *v.Index != s.index {
			return errors.New("bad block delta")
		}
		name, delta := "", ""
		switch {
		case s.blockType == "text" && v.Delta.Type == "text_delta":
			name, delta = "response.output_text.delta", v.Delta.Text
		case s.blockType == "thinking" && v.Delta.Type == "thinking_delta":
			name, delta = "response.reasoning_summary_text.delta", v.Delta.Thinking
		case s.tool && v.Delta.Type == "input_json_delta":
			name, delta = "response.function_call_arguments.delta", v.Delta.JSON
		default:
			return errors.New("unsupported block delta")
		}
		if len(delta) > maxRetainedResponseBytes-s.retained {
			return errors.New("Messages response exceeds bounded retention limit")
		}
		s.retained += len(delta)
		if s.tool {
			s.arguments.WriteString(delta)
		} else {
			s.text.WriteString(delta)
		}
		payload := map[string]any{"output_index": s.index, "item_id": s.itemID, "delta": delta}
		if name == "response.output_text.delta" {
			payload["content_index"] = 0
		}
		if name == "response.reasoning_summary_text.delta" {
			payload["summary_index"] = 0
		}
		s.pending = append(s.pending, s.emit(name, payload))
	case "content_block_stop":
		var v struct {
			Index *int `json:"index"`
		}
		if !s.block || json.Unmarshal(e.data, &v) != nil || v.Index == nil || *v.Index != s.index {
			return errors.New("bad block stop")
		}
		s.pending = append(s.pending, s.finishItem())
		s.block = false
		s.next++
	case "message_delta":
		var v struct {
			Delta struct {
				Stop *string `json:"stop_reason"`
			} `json:"delta"`
			Usage struct {
				Output *int64 `json:"output_tokens"`
			} `json:"usage"`
		}
		if !s.started || s.block || json.Unmarshal(e.data, &v) != nil || badCount(v.Usage.Output) || v.Delta.Stop == nil || s.stopReason != "" {
			return errors.New("bad message_delta")
		}
		s.output = v.Usage.Output
		if s.input != nil && s.output != nil && *s.input > math.MaxInt64-*s.output {
			return errors.New("usage total overflow")
		}
		s.stopReason = *v.Delta.Stop
	case "message_stop":
		if !s.started || s.block || s.stopped || s.stopReason == "" {
			return errors.New("bad message_stop")
		}
		if s.stopReason != "end_turn" && s.stopReason != "stop_sequence" && s.stopReason != "tool_use" && s.stopReason != "max_tokens" {
			return errors.New("unsupported stop reason")
		}
		s.stopped = true
		completeness := core.UsageUnknownCompleteness
		if s.input != nil && s.output != nil {
			completeness = core.UsageComplete
		}
		status, terminal := "completed", "response.completed"
		if s.stopReason == "max_tokens" {
			status, terminal = "incomplete", "response.incomplete"
		}
		response := map[string]any{"id": s.responseIDValue(), "object": "response", "status": status, "output": s.outputs}
		usage := map[string]any{}
		if s.input != nil {
			usage["input_tokens"] = *s.input
		}
		if s.output != nil {
			usage["output_tokens"] = *s.output
		}
		if s.input != nil && s.output != nil {
			usage["total_tokens"] = *s.input + *s.output
		}
		if len(usage) > 0 {
			response["usage"] = usage
		}
		if s.stopReason == "max_tokens" {
			response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
		}
		s.pending = append(s.pending, s.emit(terminal, map[string]any{"response": response}))
		f := core.StreamFrame{Type: core.FrameComplete, Complete: &core.CompleteFrame{Outcome: core.OutcomeSucceeded, Usage: s.usage(completeness)}}
		if s.stopReason == "max_tokens" {
			f.Complete.Outcome = core.OutcomeIncomplete
			f.Complete.Error = &core.GatewayError{Code: "output_truncated", Category: core.CategoryUnavailable, Message: "Upstream output was truncated"}
		}
		s.terminal = &f
	}
	return nil
}

func (s *messagesStream) responseIDValue() string { return "resp_" + s.responseID }

func (s *messagesStream) finishItem() []byte {
	var events [][]byte
	switch s.blockType {
	case "text":
		text := s.text.String()
		part := map[string]any{"type": "output_text", "text": text, "annotations": []any{}, "logprobs": []any{}}
		s.item["content"] = []any{part}
		s.item["status"] = "completed"
		events = append(events, s.emit("response.output_text.done", map[string]any{"output_index": s.index, "item_id": s.itemID, "content_index": 0, "text": text, "logprobs": []any{}}))
		events = append(events, s.emit("response.content_part.done", map[string]any{"output_index": s.index, "item_id": s.itemID, "content_index": 0, "part": part}))
	case "thinking":
		text := s.text.String()
		part := map[string]any{"type": "summary_text", "text": text}
		s.item["summary"] = []any{part}
		s.item["status"] = "completed"
		events = append(events, s.emit("response.reasoning_summary_text.done", map[string]any{"output_index": s.index, "item_id": s.itemID, "summary_index": 0, "text": text}))
		events = append(events, s.emit("response.reasoning_summary_part.done", map[string]any{"output_index": s.index, "item_id": s.itemID, "summary_index": 0, "part": part}))
	case "tool_use":
		arguments := s.arguments.String()
		s.item["arguments"] = arguments
		s.item["status"] = "completed"
		events = append(events, s.emit("response.function_call_arguments.done", map[string]any{"output_index": s.index, "item_id": s.itemID, "arguments": arguments}))
	}
	events = append(events, s.emit("response.output_item.done", map[string]any{"output_index": s.index, "item": s.item}))
	s.outputs = append(s.outputs, s.item)
	s.item = nil
	s.text.Reset()
	s.arguments.Reset()
	return bytes.Join(events, nil)
}
func badCount(v *int64) bool { return v != nil && *v < 0 }
func (s *messagesStream) emit(name string, payload map[string]any) []byte {
	payload["type"] = name
	payload["sequence_number"] = s.sequence
	s.sequence++
	data, _ := json.Marshal(payload)
	var out strings.Builder
	out.WriteString("event: ")
	out.WriteString(name)
	out.WriteString("\ndata: ")
	out.Write(data)
	out.WriteString("\n\n")
	return []byte(out.String())
}

var _ core.Stream = (*messagesStream)(nil)

package antigravity

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/blestafist/pestiroute/internal/core"
)

const maxSSELine = 1 << 20

type stream struct {
	r                *bufio.Reader
	body             io.ReadCloser
	mu               sync.Mutex
	closed           bool
	once             sync.Once
	closeErr         error
	phase            uint8
	pending          [][]byte
	terminal         bool
	sequence         int
	responseID       string
	messageID        string
	text             strings.Builder
	outputs          []map[string]any
	outputIndex      int
	usedCallIDs      map[string]bool
	retainedBytes    int
	messageStarted   bool
	messageIndex     int
	usage            *core.UsageReport
	sawFinish        bool
	sawTrailingUsage bool
}

func newStream(body io.ReadCloser, responseID, messageID string) *stream {
	return &stream{r: bufio.NewReader(body), body: body, responseID: responseID, messageID: messageID, messageIndex: -1}
}
func (s *stream) Close() error {
	s.once.Do(func() { s.mu.Lock(); s.closed = true; s.mu.Unlock(); s.closeErr = s.body.Close() })
	return s.closeErr
}
func (s *stream) isClosed() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.closed }
func (s *stream) Next(ctx context.Context) (core.StreamFrame, error) {
	if err := ctx.Err(); err != nil {
		_ = s.Close()
		return core.StreamFrame{}, err
	}
	if s.phase == 3 {
		return core.StreamFrame{}, io.EOF
	}
	stop := context.AfterFunc(ctx, func() { _ = s.Close() })
	defer stop()
	if s.isClosed() {
		return core.StreamFrame{}, context.Canceled
	}
	if s.phase == 2 {
		if len(s.pending) > 0 {
			return bodyFrame(s.pop()), nil
		}
		s.phase = 3
		_ = s.Close()
		return core.StreamFrame{Type: core.FrameComplete, Complete: &core.CompleteFrame{Outcome: core.OutcomeSucceeded, Usage: s.usage}}, nil
	}
	if s.phase == 0 {
		e, err := s.nextEvent(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			_ = s.Close()
			return core.StreamFrame{}, err
		}
		if err = s.consume(e); err != nil {
			_ = s.Close()
			return core.StreamFrame{}, err
		}
		s.phase = 1
		status := 200
		return core.StreamFrame{Type: core.FrameHead, Head: &core.HeadFrame{Protocol: protocol, HTTPStatus: &status, ContentType: "text/event-stream"}}, nil
	}
	if len(s.pending) > 0 {
		return bodyFrame(s.pop()), nil
	}
	for !s.terminal {
		e, err := s.nextEvent(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) && s.sawFinish {
				s.terminal = true
				break
			}
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			if ctx.Err() != nil {
				_ = s.Close()
				return core.StreamFrame{}, ctx.Err()
			}
			_ = s.Close()
			return core.StreamFrame{}, err
		}
		if err = s.consume(e); err != nil {
			_ = s.Close()
			return core.StreamFrame{}, err
		}
		if len(s.pending) > 0 {
			return bodyFrame(s.pop()), nil
		}
	}
	s.phase = 2
	if s.outputs == nil {
		s.outputs = make([]map[string]any, 0)
	}
	final := map[string]any{"id": s.responseID, "object": "response", "status": "completed", "output": s.outputs}
	if s.messageStarted {
		item := s.outputs[s.messageIndex]
		item["status"] = "completed"
		content := item["content"].([]any)[0].(map[string]any)
		s.pending = append(s.pending,
			emitEvent("response.output_text.done", map[string]any{"item_id": s.messageID, "output_index": s.messageIndex, "content_index": 0, "text": content["text"]}, &s.sequence),
			emitEvent("response.output_item.done", map[string]any{"output_index": s.messageIndex, "item": item}, &s.sequence),
		)
	}
	if s.usage != nil {
		u := map[string]int64{}
		if s.usage.InputTokens != nil {
			u["input_tokens"] = *s.usage.InputTokens
		}
		if s.usage.OutputTokens != nil {
			u["output_tokens"] = *s.usage.OutputTokens
		}
		if len(u) > 0 {
			final["usage"] = u
		}
	}
	s.pending = append(s.pending, emitEvent("response.completed", map[string]any{"response": final}, &s.sequence))
	s.phase = 2
	if len(s.pending) > 0 {
		return bodyFrame(s.pop()), nil
	}
	s.phase = 3
	_ = s.Close()
	return core.StreamFrame{Type: core.FrameComplete, Complete: &core.CompleteFrame{Outcome: core.OutcomeSucceeded, Usage: s.usage}}, nil
}
func (s *stream) pop() []byte { b := s.pending[0]; s.pending = s.pending[1:]; return b }
func bodyFrame(b []byte) core.StreamFrame {
	return core.StreamFrame{Type: core.FrameBody, Body: &core.BodyFrame{Data: b}}
}
func (s *stream) nextEvent(ctx context.Context) ([]byte, error) {
	var data []byte
	hasData := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line, err := s.readLine()
		if err != nil {
			if errors.Is(err, io.EOF) && hasData {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if line == "" {
			if hasData {
				return data, nil
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			return nil, errors.New("malformed SSE field")
		}
		value = strings.TrimPrefix(value, " ")
		if field != "data" || hasData {
			return nil, errors.New("unsupported SSE framing")
		}
		if len(value) > maxSSELine {
			return nil, errors.New("SSE event exceeds limit")
		}
		data = append(data, value...)
		hasData = true
	}
}

func (s *stream) readLine() (string, error) {
	var line []byte
	for {
		part, err := s.r.ReadSlice('\n')
		if len(line)+len(part) > maxSSELine {
			return "", errors.New("SSE line exceeds limit")
		}
		line = append(line, part...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(line) == 0 {
				return "", io.EOF
			}
			return "", io.ErrUnexpectedEOF
		}
		return string(line), nil
	}
}

func (s *stream) consume(raw []byte) error {
	if !utf8.Valid(raw) || !uniqueJSON(raw) {
		return errors.New("malformed SSE JSON")
	}
	type functionCall struct {
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
		ID   string          `json:"id"`
	}
	var outer struct {
		Response struct {
			Candidates []struct {
				FinishReason string `json:"finishReason"`
				Content      struct {
					Parts []struct {
						Text             *string         `json:"text"`
						FunctionCall     json.RawMessage `json:"functionCall"`
						Thought          *bool           `json:"thought"`
						ThoughtSignature json.RawMessage `json:"thoughtSignature"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
			Done  *bool `json:"done"`
			Usage *struct {
				Prompt     *int64 `json:"promptTokenCount"`
				Candidates *int64 `json:"candidatesTokenCount"`
			} `json:"usageMetadata"`
		} `json:"response"`
	}
	if json.Unmarshal(raw, &outer) != nil {
		return errors.New("malformed candidate frame")
	}
	resp := outer.Response
	if s.terminal {
		return errors.New("frame after terminal marker")
	}
	if s.sawFinish {
		if s.sawTrailingUsage || resp.Done != nil || len(resp.Candidates) != 0 || resp.Usage == nil || !usageOnlyFrame(raw) {
			return errors.New("frame after candidate finish reason")
		}
		if err := s.setUsage(resp.Usage); err != nil {
			return err
		}
		s.sawTrailingUsage = true
		return nil
	}
	if resp.Done != nil && *resp.Done {
		if len(resp.Candidates) > 0 {
			return errors.New("terminal frame contains unsupported candidate content")
		}
		if resp.Usage != nil {
			if (resp.Usage.Prompt != nil && *resp.Usage.Prompt < 0) || (resp.Usage.Candidates != nil && *resp.Usage.Candidates < 0) {
				return errors.New("invalid usage")
			}
			if resp.Usage.Prompt != nil || resp.Usage.Candidates != nil {
				completeness := core.UsagePartial
				if resp.Usage.Prompt != nil && resp.Usage.Candidates != nil {
					completeness = core.UsageComplete
				}
				s.usage = &core.UsageReport{InputTokens: resp.Usage.Prompt, OutputTokens: resp.Usage.Candidates, Source: core.UsageProvider, Completeness: completeness}
			}
		}
		s.terminal = true
		return nil
	}
	type parsedPart struct {
		text   string
		call   *functionCall
		args   string
		callID string
		itemID string
	}
	var parts []parsedPart
	finished := false
	retainedBytes := s.retainedBytes
	var stagedCallIDs map[string]bool
	for _, candidate := range resp.Candidates {
		if candidate.FinishReason != "" {
			if candidate.FinishReason != "STOP" {
				return errors.New("unsupported candidate finish reason")
			}
			finished = true
		}
		for _, part := range candidate.Content.Parts {
			if part.Thought != nil && *part.Thought || len(part.ThoughtSignature) != 0 {
				return errors.New("unsupported reasoning signature")
			}
			if part.Text != nil && len(part.FunctionCall) != 0 {
				return errors.New("candidate part has conflicting content")
			}
			if part.Text != nil {
				if *part.Text == "" || !utf8.ValidString(*part.Text) || len(*part.Text) > maxRequestBytes-retainedBytes {
					return errors.New("invalid or oversized candidate text")
				}
				retainedBytes += len(*part.Text)
				parts = append(parts, parsedPart{text: *part.Text})
				continue
			}
			if len(part.FunctionCall) != 0 {
				var fields map[string]json.RawMessage
				if json.Unmarshal(part.FunctionCall, &fields) != nil || fields == nil || fields["name"] == nil || fields["args"] == nil {
					return errors.New("invalid function call")
				}
				for key := range fields {
					if key != "name" && key != "args" && key != "id" {
						return errors.New("unsupported function-call field")
					}
				}
				call := new(functionCall)
				if json.Unmarshal(part.FunctionCall, call) != nil {
					return errors.New("invalid function call")
				}
				if !validFunctionName(call.Name) || len(call.Args) == 0 || !uniqueJSON(call.Args) || !utf8.Valid(call.Args) || len(call.Args) > maxRequestBytes-retainedBytes {
					return errors.New("invalid or oversized function-call arguments")
				}
				trimmed := strings.TrimSpace(string(call.Args))
				if len(trimmed) == 0 || trimmed[0] != '{' {
					return errors.New("function-call arguments must be an object")
				}
				var object map[string]json.RawMessage
				if json.Unmarshal(call.Args, &object) != nil || object == nil {
					return errors.New("function-call arguments must be an object")
				}
				arguments := string(call.Args)
				if idRaw, present := fields["id"]; present && (json.Unmarshal(idRaw, &call.ID) != nil || call.ID == "") {
					return errors.New("invalid function-call ID")
				}
				if call.ID != "" {
					if len(call.ID) > 256 || !utf8.ValidString(call.ID) || strings.TrimSpace(call.ID) != call.ID {
						return errors.New("invalid function-call ID")
					}
				}
				callIndex := len(s.usedCallIDs) + len(stagedCallIDs)
				callID := call.ID
				if callID == "" {
					callID = "call_" + s.responseID + "_" + strconv.Itoa(callIndex)
				}
				if s.usedCallIDs[callID] || stagedCallIDs[callID] {
					return errors.New("duplicate function-call ID")
				}
				if stagedCallIDs == nil {
					stagedCallIDs = make(map[string]bool)
				}
				stagedCallIDs[callID] = true
				itemID := "fc_" + s.responseID + "_" + strconv.Itoa(callIndex)
				retainedBytes += len(arguments)
				parts = append(parts, parsedPart{call: call, args: arguments, callID: callID, itemID: itemID})
				continue
			}
			return errors.New("candidate part has no supported content")
		}
	}
	if resp.Usage != nil {
		if err := s.setUsage(resp.Usage); err != nil {
			return err
		}
	}
	if finished {
		s.sawFinish = true
	}
	if len(stagedCallIDs) > 0 {
		if s.usedCallIDs == nil {
			s.usedCallIDs = make(map[string]bool, len(stagedCallIDs))
		}
		for id := range stagedCallIDs {
			s.usedCallIDs[id] = true
		}
	}
	if len(resp.Candidates) == 0 {
		if resp.Usage != nil {
			return nil
		}
		return errors.New("candidate frame has no text")
	}
	for _, part := range parts {
		if s.sequence == 0 {
			s.pending = append(s.pending, emitEvent("response.created", map[string]any{"response": map[string]any{"id": s.responseID, "object": "response", "status": "in_progress", "output": []any{}}}, &s.sequence))
		}
		if part.call == nil {
			if !s.messageStarted {
				item := map[string]any{"id": s.messageID, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{map[string]any{"type": "output_text", "text": "", "annotations": []any{}}}}
				s.outputs = append(s.outputs, item)
				s.messageIndex = s.outputIndex
				s.outputIndex++
				s.messageStarted = true
				s.pending = append(s.pending, emitEvent("response.output_item.added", map[string]any{"output_index": s.messageIndex, "item": item}, &s.sequence))
			}
			s.text.WriteString(part.text)
			content := s.outputs[s.messageIndex]["content"].([]any)[0].(map[string]any)
			content["text"] = s.text.String()
			s.pending = append(s.pending, emitEvent("response.output_text.delta", map[string]any{"item_id": s.messageID, "output_index": s.messageIndex, "content_index": 0, "delta": part.text}, &s.sequence))
			continue
		}
		index := s.outputIndex
		s.outputIndex++
		item := map[string]any{"id": part.itemID, "type": "function_call", "call_id": part.callID, "name": part.call.Name, "arguments": part.args, "status": "completed"}
		s.outputs = append(s.outputs, item)
		s.pending = append(s.pending,
			emitEvent("response.output_item.added", map[string]any{"output_index": index, "item": map[string]any{"id": part.itemID, "type": "function_call", "call_id": part.callID, "name": part.call.Name, "arguments": "", "status": "in_progress"}}, &s.sequence),
			emitEvent("response.function_call_arguments.delta", map[string]any{"item_id": part.itemID, "output_index": index, "delta": part.args}, &s.sequence),
			emitEvent("response.function_call_arguments.done", map[string]any{"item_id": part.itemID, "output_index": index, "arguments": part.args}, &s.sequence),
			emitEvent("response.output_item.done", map[string]any{"output_index": index, "item": item}, &s.sequence),
		)
	}
	s.retainedBytes = retainedBytes
	if len(s.pending) == 0 && !finished && resp.Usage == nil {
		return errors.New("candidate frame has no text")
	}
	return nil
}

func usageOnlyFrame(raw []byte) bool {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(raw, &envelope) != nil || len(envelope) != 1 {
		return false
	}
	responseRaw, ok := envelope["response"]
	if !ok {
		return false
	}
	var response map[string]json.RawMessage
	if json.Unmarshal(responseRaw, &response) != nil || len(response) != 1 {
		return false
	}
	usageRaw, ok := response["usageMetadata"]
	if !ok {
		return false
	}
	var usage map[string]json.RawMessage
	if json.Unmarshal(usageRaw, &usage) != nil || usage == nil {
		return false
	}
	for key := range usage {
		if key != "promptTokenCount" && key != "candidatesTokenCount" {
			return false
		}
	}
	return true
}

func (s *stream) setUsage(usage *struct {
	Prompt     *int64 `json:"promptTokenCount"`
	Candidates *int64 `json:"candidatesTokenCount"`
}) error {
	if (usage.Prompt != nil && *usage.Prompt < 0) || (usage.Candidates != nil && *usage.Candidates < 0) {
		return errors.New("invalid usage")
	}
	if usage.Prompt != nil || usage.Candidates != nil {
		completeness := core.UsagePartial
		if usage.Prompt != nil && usage.Candidates != nil {
			completeness = core.UsageComplete
		}
		s.usage = &core.UsageReport{InputTokens: usage.Prompt, OutputTokens: usage.Candidates, Source: core.UsageProvider, Completeness: completeness}
	}
	return nil
}
func emitEvent(name string, payload map[string]any, seq *int) []byte {
	payload["type"] = name
	payload["sequence_number"] = *seq
	*seq++
	b, _ := json.Marshal(payload)
	return []byte("event: " + name + "\ndata: " + string(b) + "\n\n")
}

var _ core.Stream = (*stream)(nil)

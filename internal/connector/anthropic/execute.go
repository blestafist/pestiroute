package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/blestafist/pestiroute/internal/core"
)

type messagesStream struct {
	mu         sync.Mutex
	body       io.ReadCloser
	reader     *messagesSSEReader
	cancel     context.CancelFunc
	ctx        context.Context
	phase      uint8
	pending    [][]byte
	terminal   *core.StreamFrame
	emitter    *responsesEmitter
	started    bool
	block      bool
	tool       bool
	blockIndex int
	nextIndex  int
	stop       string
	failure    *core.GatewayError
	input      *int64
	output     *int64
	cached     *int64
	closed     bool
	close      sync.Once
}

func newMessagesStream(ctx context.Context, cancel context.CancelFunc, body io.ReadCloser) core.Stream {
	return &messagesStream{ctx: ctx, cancel: cancel, body: body, reader: newMessagesSSEReader(body)}
}

func (s *messagesStream) Close() error {
	s.close.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		s.cancel()
		_ = s.body.Close()
	})
	return nil
}

func (s *messagesStream) Next(ctx context.Context) (core.StreamFrame, error) {
	if err := ctx.Err(); err != nil {
		_ = s.Close()
		return core.StreamFrame{}, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return core.StreamFrame{}, context.Canceled
	}
	if s.phase == 3 {
		s.mu.Unlock()
		return core.StreamFrame{}, io.EOF
	}
	if s.phase == 0 {
		s.phase = 1
		status := 200
		s.mu.Unlock()
		return core.StreamFrame{Type: core.FrameHead, Head: &core.HeadFrame{Protocol: protocol, ContentType: "text/event-stream", HTTPStatus: &status}}, nil
	}
	if len(s.pending) > 0 {
		data := s.pending[0]
		s.pending = s.pending[1:]
		s.mu.Unlock()
		return bodyFrame(data), nil
	}
	if s.phase == 2 && len(s.pending) == 0 {
		frame := *s.terminal
		s.terminal = nil
		s.phase = 3
		s.mu.Unlock()
		s.cancel()
		_ = s.body.Close()
		return frame, nil
	}
	s.mu.Unlock()
	for {
		stop := context.AfterFunc(ctx, func() { _ = s.Close() })
		event, err := s.reader.next(s.ctx)
		stop()
		if ctx.Err() != nil {
			_ = s.Close()
			return core.StreamFrame{}, ctx.Err()
		}
		if s.ctx.Err() != nil {
			_ = s.Close()
			return core.StreamFrame{}, context.Canceled
		}
		if err != nil {
			if s.ctx.Err() != nil || ctx.Err() != nil {
				_ = s.Close()
				return core.StreamFrame{}, context.Canceled
			}
			if err == io.EOF {
				return s.incomplete(), nil
			}
			return s.failed(gatewayFailure("invalid_response", core.CategoryUnavailable, "Upstream stream failed")), nil
		}
		if err = s.consume(event); err != nil {
			failure := s.failure
			if failure == nil {
				failure = gatewayFailure("invalid_response", core.CategoryUnavailable, "Upstream response was invalid")
			}
			return s.failed(failure), nil
		}
		s.mu.Lock()
		if len(s.pending) > 0 {
			data := s.pending[0]
			s.pending = s.pending[1:]
			s.mu.Unlock()
			return bodyFrame(data), nil
		}
		s.mu.Unlock()
	}
}

func (s *messagesStream) incomplete() core.StreamFrame {
	s.mu.Lock()
	s.phase = 2
	s.mu.Unlock()
	s.cancel()
	_ = s.body.Close()
	usage := s.usage(core.UsagePartial)
	done := &core.CompleteFrame{
		Outcome: core.OutcomeIncomplete,
		Error:   &core.GatewayError{Code: "incomplete_response", Category: core.CategoryUnavailable, Message: "Upstream response was incomplete"},
		Usage:   usage,
	}
	s.terminal = &core.StreamFrame{Type: core.FrameComplete, Complete: done}
	if s.emitter != nil {
		if event, err := s.emitter.Incomplete(s.input, s.output, s.cached); err == nil {
			s.pending = append(s.pending, event)
		}
	}
	if len(s.pending) > 0 {
		frame := bodyFrame(s.pending[0])
		s.pending = s.pending[1:]
		return frame
	}
	s.phase = 3
	return *s.terminal
}

func (s *messagesStream) failed(failure *core.GatewayError) core.StreamFrame {
	s.mu.Lock()
	s.phase = 2
	s.failure = failure
	s.mu.Unlock()
	if s.emitter != nil {
		if event, err := s.emitter.Failed(failure.Code, failure.Message, s.input, s.output, s.cached); err == nil {
			s.pending = append(s.pending, event)
		}
	}
	s.cancel()
	_ = s.body.Close()
	done := &core.CompleteFrame{Outcome: core.OutcomeFailed, Error: failure, Usage: s.usage(core.UsagePartial)}
	s.terminal = &core.StreamFrame{Type: core.FrameComplete, Complete: done}
	if len(s.pending) == 0 {
		s.phase = 3
		return *s.terminal
	}
	frame := bodyFrame(s.pending[0])
	s.pending = s.pending[1:]
	return frame
}

func (s *messagesStream) usage(completeness core.UsageCompleteness) *core.UsageReport {
	source := core.UsageProvider
	if s.input == nil && s.output == nil && s.cached == nil {
		source = core.UsageUnknown
	}
	return &core.UsageReport{InputTokens: s.input, OutputTokens: s.output, CachedTokens: s.cached, Source: source, Completeness: completeness}
}

func bodyFrame(data []byte) core.StreamFrame {
	return core.StreamFrame{Type: core.FrameBody, Body: &core.BodyFrame{Data: data}}
}

func (s *messagesStream) consume(event messagesSSEEvent) error {
	switch event.typeName {
	case "ping":
		return nil
	case "error":
		s.failure = classifyStreamError(event.data)
		return errors.New("Anthropic stream reported an error")
	case "message_start":
		if s.started {
			return errResponsesLifecycle
		}
		var v struct {
			Message *struct {
				Usage *struct {
					Input        *int64 `json:"input_tokens"`
					CacheRead    *int64 `json:"cache_read_input_tokens"`
					CacheCreated *int64 `json:"cache_creation_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(event.data, &v) != nil || v.Message == nil || v.Message.Usage == nil || invalidCount(v.Message.Usage.Input) || invalidCount(v.Message.Usage.CacheRead) || invalidCount(v.Message.Usage.CacheCreated) {
			return errors.New("invalid Anthropic message_start")
		}
		input, err := addCounts(v.Message.Usage.Input, v.Message.Usage.CacheRead, v.Message.Usage.CacheCreated)
		if err != nil {
			return errors.New("invalid Anthropic message_start")
		}
		e, err := newResponsesEmitter()
		if err != nil {
			return err
		}
		s.emitter, s.started, s.input, s.cached = e, true, input, v.Message.Usage.CacheRead
		frames, err := e.StartResponse()
		if err == nil {
			s.pending = append(s.pending, frames...)
		}
		return err
	case "content_block_start":
		var v struct {
			Index int `json:"index"`
			Block struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
		}
		if !s.started || s.block || s.stop != "" || json.Unmarshal(event.data, &v) != nil || v.Index != s.nextIndex || v.Index >= 64 {
			return errors.New("invalid Anthropic content block start")
		}
		switch v.Block.Type {
		case "text":
			frames, err := s.emitter.StartText()
			if err != nil {
				return err
			}
			s.pending = append(s.pending, frames...)
		case "tool_use":
			frame, err := s.emitter.StartTool(v.Block.ID, v.Block.Name)
			if err != nil {
				return fmt.Errorf("invalid Anthropic tool_use block: %w", err)
			}
			s.tool = true
			s.pending = append(s.pending, frame)
		default:
			return errors.New("unsupported Anthropic content block type")
		}
		s.block = true
		s.blockIndex = v.Index
	case "content_block_delta":
		var v struct {
			Index int `json:"index"`
			Delta struct {
				Type        string  `json:"type"`
				Text        string  `json:"text"`
				PartialJSON *string `json:"partial_json"`
			} `json:"delta"`
		}
		if !s.block || json.Unmarshal(event.data, &v) != nil || v.Index != s.blockIndex {
			return errors.New("invalid Anthropic content block delta")
		}
		var frame []byte
		var err error
		if s.tool && v.Delta.Type == "input_json_delta" && v.Delta.PartialJSON != nil {
			frame, err = s.emitter.ToolDelta(*v.Delta.PartialJSON)
		} else if !s.tool && v.Delta.Type == "text_delta" {
			frame, err = s.emitter.Delta(v.Delta.Text)
		} else {
			return errors.New("unexpected Anthropic content block delta type")
		}
		if err == nil {
			s.pending = append(s.pending, frame)
		}
		return err
	case "content_block_stop":
		var v struct {
			Index int `json:"index"`
		}
		if !s.block || json.Unmarshal(event.data, &v) != nil || v.Index != s.blockIndex {
			return errors.New("invalid Anthropic content block stop")
		}
		if s.tool {
			frames, err := s.emitter.FinishTool()
			if err != nil {
				return err
			}
			s.pending = append(s.pending, frames...)
		}
		s.block = false
		s.nextIndex++
	case "message_delta":
		var v struct {
			Delta struct {
				StopReason *string `json:"stop_reason"`
			} `json:"delta"`
			Usage struct {
				Output *int64 `json:"output_tokens"`
			} `json:"usage"`
		}
		if !s.started || s.block || s.nextIndex == 0 || s.stop != "" || json.Unmarshal(event.data, &v) != nil || invalidCount(v.Usage.Output) {
			return errors.New("invalid Anthropic message_delta")
		}
		if v.Delta.StopReason != nil {
			s.stop = *v.Delta.StopReason
		}
		s.output = v.Usage.Output
	case "message_stop":
		if !s.started || s.nextIndex == 0 || s.block || s.stop == "" || s.phase != 1 {
			return errors.New("invalid Anthropic message_stop")
		}
		if s.stop != "end_turn" && s.stop != "stop_sequence" && s.stop != "tool_use" && s.stop != "max_tokens" {
			return errors.New("unsupported Anthropic stop reason")
		}
		frames, err := s.emitter.Finish(s.stop, s.input, s.output, s.cached)
		if err != nil {
			return err
		}
		s.pending = append(s.pending, frames...)
		outcome := core.OutcomeSucceeded
		var terminalErr *core.GatewayError
		if s.stop == "max_tokens" {
			outcome = core.OutcomeIncomplete
			terminalErr = &core.GatewayError{Code: "output_truncated", Category: core.CategoryUnavailable, Message: "Upstream output was truncated"}
		}
		complete := core.StreamFrame{Type: core.FrameComplete, Complete: &core.CompleteFrame{Outcome: outcome, Error: terminalErr, Usage: s.usage(core.UsageComplete)}}
		s.terminal = &complete
		s.phase = 2
	default:
		return errors.New("unsupported Anthropic event")
	}
	return nil
}

func classifyStreamError(data []byte) *core.GatewayError {
	var wire struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	_ = json.Unmarshal(data, &wire)
	if wire.Error.Type != "" {
		wire.Type = wire.Error.Type
	}
	category, code, message := core.CategoryUnavailable, "provider_failure", "Upstream reported a failure"
	switch wire.Type {
	case "rate_limit_error":
		category, code = core.CategoryRateLimited, "upstream_rate_limited"
	case "overloaded_error":
		code = "upstream_overloaded"
	case "api_error":
		code = "upstream_api_error"
	}
	return gatewayFailure(code, category, message)
}

func gatewayFailure(code string, category core.ErrorCategory, message string) *core.GatewayError {
	return &core.GatewayError{Code: code, Category: category, Message: message}
}

func invalidCount(v *int64) bool { return v != nil && *v < 0 }

func addCounts(counts ...*int64) (*int64, error) {
	var total int64
	for _, count := range counts {
		if count == nil {
			return nil, nil
		}
		if *count < 0 || total > int64(^uint64(0)>>1)-*count {
			return nil, errors.New("usage count overflow")
		}
		total += *count
	}
	return &total, nil
}

var _ core.Stream = (*messagesStream)(nil)

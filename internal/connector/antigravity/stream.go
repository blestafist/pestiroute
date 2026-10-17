package antigravity

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"

	"github.com/blestafist/pestiroute/internal/core"
)

const maxSSELine = 1 << 20

type stream struct {
	r          *bufio.Reader
	body       io.ReadCloser
	mu         sync.Mutex
	closed     bool
	once       sync.Once
	closeErr   error
	phase      uint8
	pending    [][]byte
	terminal   bool
	sequence   int
	responseID string
	text       strings.Builder
	usage      *core.UsageReport
}

func newStream(body io.ReadCloser) *stream {
	return &stream{r: bufio.NewReader(body), body: body, responseID: "resp_synthetic-request-001"}
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
		s.phase = 3
		_ = s.Close()
		return core.StreamFrame{Type: core.FrameComplete, Complete: &core.CompleteFrame{Outcome: core.OutcomeSucceeded, Usage: s.usage}}, nil
	}
	if s.phase == 0 {
		e, err := s.nextEvent(ctx)
		if err != nil {
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
	final := map[string]any{"id": s.responseID, "object": "response", "status": "completed", "output": []any{map[string]any{"id": "msg_synthetic", "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": s.text.String(), "annotations": []any{}}}}}}
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
			return "", io.ErrUnexpectedEOF
		}
		return string(line), nil
	}
}

func (s *stream) consume(raw []byte) error {
	if !uniqueJSON(raw) {
		return errors.New("malformed SSE JSON")
	}
	var outer struct {
		Response struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text *string `json:"text"`
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
	if len(resp.Candidates) == 0 {
		return errors.New("candidate frame has no text")
	}
	for _, candidate := range resp.Candidates {
		for _, part := range candidate.Content.Parts {
			if part.Text == nil || *part.Text == "" || len(*part.Text) > maxRequestBytes-s.text.Len() {
				return errors.New("invalid or oversized candidate text")
			}
			s.text.WriteString(*part.Text)
			if s.sequence == 0 {
				s.pending = append(s.pending, emitEvent("response.created", map[string]any{"response": map[string]any{"id": s.responseID, "object": "response", "status": "in_progress", "output": []any{}}}, &s.sequence))
			}
			s.pending = append(s.pending, emitEvent("response.output_text.delta", map[string]any{"item_id": "msg_synthetic", "output_index": 0, "content_index": 0, "delta": *part.Text}, &s.sequence))
		}
	}
	if len(s.pending) == 0 {
		return errors.New("candidate frame has no text")
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

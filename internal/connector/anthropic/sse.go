package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

const maxSSEBytes = 1 << 20

var errSSETooLarge = errors.New("Anthropic SSE line or event exceeds 1 MiB")

type messagesSSEEvent struct {
	typeName string
	data     []byte
}

// messagesSSEReader frames one event at a time; its only retained input is a
// bounded line/event and fixed-size read buffer. It only checks payload framing
// and event type; provider payload translation belongs to later Connector work.
type messagesSSEReader struct {
	r       io.ReadCloser
	buf     [4096]byte
	pos, n  int
	readErr error
	line    []byte
	data    []byte
	event   string
	hasData bool
	cr      bool
}

// The caller owns r and must close it on normal completion; cancellation closes
// it to interrupt a blocked read. A response body is the intended input.
func newMessagesSSEReader(r io.ReadCloser) *messagesSSEReader { return &messagesSSEReader{r: r} }

func (s *messagesSSEReader) next(ctx context.Context) (messagesSSEEvent, error) {
	stop := context.AfterFunc(ctx, func() {
		_ = s.r.Close()
	})
	defer stop()
	for {
		if err := ctx.Err(); err != nil {
			return messagesSSEEvent{}, err
		}
		if s.pos == s.n {
			if s.readErr != nil {
				if s.readErr != io.EOF {
					return messagesSSEEvent{}, s.readErr
				}
				// SSE dispatches only on a blank line. EOF drops any pending
				// partial line/event rather than manufacturing a complete event.
				return messagesSSEEvent{}, io.EOF
			}
			s.n, s.readErr = s.r.Read(s.buf[:])
			s.pos = 0
			if s.n == 0 && s.readErr == nil {
				return messagesSSEEvent{}, io.ErrNoProgress
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
			if err := s.endLine(); err != nil {
				return messagesSSEEvent{}, err
			}
			if b == '\r' {
				s.cr = true
			}
			if blank {
				event, err := s.endEvent()
				if err != nil || event.typeName != "" {
					return event, err
				}
			}
			continue
		}
		if len(s.line) == maxSSEBytes {
			return messagesSSEEvent{}, errSSETooLarge
		}
		s.line = append(s.line, b)
	}
}

func (s *messagesSSEReader) endLine() error {
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
			return errors.New("invalid UTF-8 in Anthropic SSE event type")
		}
		s.event = string(value)
	case "data":
		separator := 0
		if s.hasData {
			separator = 1
		}
		if len(s.event)+len(s.data)+len(value)+separator > maxSSEBytes {
			return errSSETooLarge
		}
		if s.hasData {
			s.data = append(s.data, '\n')
		}
		s.data = append(s.data, value...)
		s.hasData = true
	}
	return nil
}

func (s *messagesSSEReader) endEvent() (messagesSSEEvent, error) {
	if !s.hasData {
		s.event, s.data = "", nil
		return messagesSSEEvent{}, nil
	}
	if !utf8.Valid(s.data) {
		return messagesSSEEvent{}, errors.New("invalid UTF-8 in Anthropic SSE data")
	}
	name := s.event
	var payload struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(s.data, &payload); err != nil {
		return messagesSSEEvent{}, fmt.Errorf("malformed Anthropic SSE data: %w", err)
	}
	if name == "" {
		name = payload.Type
	}
	if payload.Type != "" && name != payload.Type {
		return messagesSSEEvent{}, errors.New("Anthropic SSE event type does not match data")
	}
	if !supportedMessagesEvent(name) {
		// Harmless notifications without semantic data are ignored; an unknown
		// typed payload may carry meaning and is therefore rejected conservatively.
		s.resetEvent()
		return messagesSSEEvent{}, fmt.Errorf("unsupported Anthropic SSE event %q", name)
	}
	e := messagesSSEEvent{typeName: name, data: append([]byte(nil), s.data...)}
	s.resetEvent()
	return e, nil
}

func (s *messagesSSEReader) resetEvent() {
	s.event, s.data, s.hasData = "", nil, false
}

func supportedMessagesEvent(name string) bool {
	switch name {
	case "message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop", "ping", "error":
		return true
	default:
		return false
	}
}

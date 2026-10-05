package codex

import (
	"errors"
	"unicode/utf8"
)

const (
	maxSSEEventBytes  = 1 << 20
	maxSSEScalarBytes = 4 << 10
)

var errInvalidSSE = errors.New("invalid or oversized SSE framing")

// sseObserver validates framing without retaining or rewriting the response.
// Its only variable-size buffer is one bounded line of the current event.
type sseObserver struct {
	line       []byte
	eventBytes int
	dataLines  int
	eventName  string
	lastEvent  string
	events     uint64
	cr         bool
	err        error
}

func (o *sseObserver) feed(p []byte) {
	for _, b := range p {
		if o.err != nil {
			return
		}
		if o.cr {
			o.cr = false
			if b == '\n' {
				continue
			}
		}
		if o.eventBytes == maxSSEEventBytes {
			o.err = errInvalidSSE
			return
		}
		o.eventBytes++
		if b == '\r' || b == '\n' {
			o.endLine()
			if b == '\r' {
				o.cr = true
			}
			continue
		}
		if len(o.line) >= maxSSEEventBytes {
			o.err = errInvalidSSE
			return
		}
		o.line = append(o.line, b)
	}
}

func (o *sseObserver) endLine() {
	line := o.line
	o.line = nil
	if len(line) == 0 {
		o.endEvent()
		return
	}
	if line[0] == ':' { // Comment / keepalive.
		if len(line) > maxSSEScalarBytes || !utf8.Valid(line) {
			o.err = errInvalidSSE
		}
		return
	}
	field, value, hasValue := line, []byte(nil), false
	for i, b := range line {
		if b == ':' {
			field, value, hasValue = line[:i], line[i+1:], true
			if len(value) > 0 && value[0] == ' ' {
				value = value[1:]
			}
			break
		}
	}
	if !utf8.Valid(field) || !utf8.Valid(value) {
		o.err = errInvalidSSE
		return
	}
	switch string(field) {
	case "data":
		// SSE data may be large, but remains bounded by the enclosing event.
		o.dataLines++
	case "event":
		if len(value) > maxSSEScalarBytes {
			o.err = errInvalidSSE
			return
		}
		o.eventName = string(value)
	case "id", "retry":
		if len(value) > maxSSEScalarBytes {
			o.err = errInvalidSSE
			return
		}
	default:
		// Unknown SSE fields remain opaque and are only framing-validated.
		if len(field) > maxSSEScalarBytes || hasValue && len(value) > maxSSEScalarBytes {
			o.err = errInvalidSSE
		}
	}
}

func (o *sseObserver) endEvent() {
	if o.dataLines > 0 {
		o.events++
		o.lastEvent = o.eventName
	}
	o.dataLines = 0
	o.eventName = ""
	o.eventBytes = 0
}

func (o *sseObserver) finish() error {
	if o.err != nil {
		return o.err
	}
	if len(o.line) != 0 || o.eventBytes != 0 {
		return errInvalidSSE
	}
	return nil
}

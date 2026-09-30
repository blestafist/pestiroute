package responses

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	"github.com/blestafist/pestiroute/internal/core"
)

// ExecuteSSE performs a single native streaming attempt. Observation is inline
// with the consumer's reads, so an upstream read never outruns downstream writes.
func (t *Transport) ExecuteSSE(ctx context.Context, in core.ExecutionRequest) (core.ExecutionResponse, *core.GatewayError) {
	if in.Payload.Protocol != protocol {
		return core.ExecutionResponse{}, gatewayError("unsupported_protocol", core.CategoryUnsupportedFeature, "Unsupported response protocol")
	}
	requestCtx, cancel := context.WithCancel(ctx)
	resp, err := t.Do(requestCtx, in)
	if err != nil {
		cancel()
		return core.ExecutionResponse{}, transportError(err)
	}
	for _, value := range resp.Header.Values("Content-Encoding") {
		for _, token := range strings.Split(value, ",") {
			if !strings.EqualFold(strings.TrimSpace(token), "identity") {
				resp.Body.Close()
				cancel()
				return core.ExecutionResponse{}, gatewayError("unsupported_response_encoding", core.CategoryUnavailable, fmt.Sprintf("Unsupported upstream response encoding (HTTP %d)", resp.StatusCode))
			}
		}
	}
	headers := resp.Header.Clone()
	blocked := map[string]bool{"connection": true, "keep-alive": true, "proxy-authenticate": true, "proxy-authorization": true, "te": true, "trailer": true, "transfer-encoding": true, "upgrade": true, "set-cookie": true, "authorization": true, "proxy-connection": true}
	for _, v := range headers.Values("Connection") {
		for _, field := range strings.Split(v, ",") {
			blocked[strings.ToLower(strings.TrimSpace(field))] = true
		}
	}
	for name := range headers {
		if blocked[strings.ToLower(name)] {
			delete(headers, name)
		}
	}
	status := resp.StatusCode
	head := &core.HeadFrame{Protocol: protocol, ContentType: resp.Header.Get("Content-Type"), HTTPStatus: &status, Headers: headers}
	if status < 200 || status >= 300 {
		head.Error = rejectionError(status, resp.Header.Get("Retry-After"))
	}
	return core.ExecutionResponse{Stream: &sseStream{body: resp.Body, cancel: cancel, requestCtx: requestCtx, head: head}}, nil
}

type sseStream struct {
	mu         sync.Mutex
	body       io.ReadCloser
	cancel     context.CancelFunc
	requestCtx context.Context
	head       *core.HeadFrame
	phase      int
	pending    error
	observer   sseObserver
}

func (s *sseStream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase == 3 {
		return nil
	}
	s.phase = 3
	s.cancel()
	return s.body.Close()
}
func (s *sseStream) Next(ctx context.Context) (core.StreamFrame, error) {
	if err := ctx.Err(); err != nil {
		s.Close()
		return core.StreamFrame{}, err
	}
	s.mu.Lock()
	switch s.phase {
	case 0:
		s.phase = 1
		h := s.head
		s.mu.Unlock()
		return core.StreamFrame{Type: core.FrameHead, Head: h}, nil
	case 2:
		s.mu.Unlock()
		return core.StreamFrame{}, io.EOF
	case 3:
		s.mu.Unlock()
		return core.StreamFrame{}, context.Canceled
	}
	pending := s.pending
	s.mu.Unlock()
	stop := context.AfterFunc(ctx, func() { s.Close() })
	defer stop()
	buf := make([]byte, chunkSize)
	var n int
	var err error
	if pending != nil {
		err = pending
	} else {
		for n == 0 && err == nil && ctx.Err() == nil {
			n, err = s.body.Read(buf)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx.Err() != nil {
		return core.StreamFrame{}, ctx.Err()
	}
	if s.phase == 3 {
		return core.StreamFrame{}, context.Canceled
	}
	if n > 0 {
		s.pending = err
		s.observer.feed(buf[:n])
		return core.StreamFrame{Type: core.FrameBody, Body: &core.BodyFrame{Data: buf[:n]}}, nil
	}
	s.phase = 2
	s.body.Close()
	done := s.observer.complete(err, s.requestCtx.Err(), s.head.Error)
	s.cancel()
	return core.StreamFrame{Type: core.FrameComplete, Complete: done}, nil
}

// SSE framing retains only the short field prefix and observed event name.
// JSON is fed one byte at a time; even a huge data line is never retained.
type sseObserver struct {
	prefix    []byte
	field     byte // 0 undecided, 1 data, 2 event, 3 ignored
	lineLen   int
	skipSpace bool
	cr        bool
	data      bool
	event     []byte
	json      tokenObserver
	terminal  *core.CompleteFrame
	invalid   bool
}

const scalarLimit = 4096

func (o *sseObserver) feed(p []byte) {
	for _, b := range p {
		if o.cr {
			o.cr = false
			if b == '\n' {
				continue
			}
		}
		if b == '\r' || b == '\n' {
			o.endLine()
			if b == '\r' {
				o.cr = true
			}
			continue
		}
		o.lineLen++
		if o.field == 0 {
			if len(o.prefix) < 7 {
				o.prefix = append(o.prefix, b)
			}
			name := string(o.prefix)
			if name == "data:" || name == "data: " {
				o.field = 1
				o.data = true
				o.skipSpace = name == "data:"
				continue
			}
			if name == "event:" || name == "event: " {
				o.field = 2
				o.skipSpace = name == "event:"
				continue
			}
			if len(o.prefix) >= 7 || (!strings.HasPrefix("data:", name) && !strings.HasPrefix("event:", name) && !strings.HasPrefix("data: ", name) && !strings.HasPrefix("event: ", name)) {
				o.field = 3
			}
			continue
		}
		if o.skipSpace {
			o.skipSpace = false
			if b == ' ' {
				continue
			}
		}
		if o.field == 1 {
			o.json.feed(b)
		}
		if o.field == 2 {
			if len(o.event) < scalarLimit {
				o.event = append(o.event, b)
			} else {
				o.invalid = true
			}
		}
	}
}
func (o *sseObserver) endLine() {
	if o.lineLen == 0 {
		o.endEvent()
	} else if o.field == 1 {
		o.json.feed('\n')
	}
	o.prefix = nil
	o.field = 0
	o.lineLen = 0
	o.skipSpace = false
}
func (o *sseObserver) endEvent() {
	if o.data && o.terminal != nil {
		// A completed attempt cannot acquire another event or resume output.
		// Keep this violation sticky even if more terminal events follow.
		o.terminal = &core.CompleteFrame{Outcome: core.OutcomeIncomplete, Error: gatewayError("invalid_response", core.CategoryUnavailable, "Upstream sent data after its terminal event"), Usage: unknownUsage()}
	} else if o.data {
		kind := string(o.event)
		if kind == "" {
			kind = o.json.value("type")
		}
		if kind == "response.completed" || kind == "response.failed" || kind == "response.incomplete" || kind == "error" {
			unknown := unknownUsage()
			done := &core.CompleteFrame{Outcome: core.OutcomeIncomplete, Error: gatewayError("invalid_response", core.CategoryUnavailable, "Upstream terminal event was invalid"), Usage: unknown}
			if !o.invalid && o.json.valid() && o.json.value("type") != "" && (kind == o.json.value("type") || len(o.event) == 0) {
				status := o.json.value("response.status")
				switch {
				case kind == "error" || kind == "response.failed" || o.json.value("response.error") != "null" && o.json.has("response.error"):
					done.Outcome = core.OutcomeFailed
					done.Error = gatewayError("provider_failure", core.CategoryUnavailable, "Upstream reported a failure")
				case kind == "response.completed" && status == "completed":
					done.Outcome = core.OutcomeSucceeded
					done.Error = nil
				case kind == "response.incomplete" || status == "incomplete":
					done.Error = gatewayError("incomplete_response", core.CategoryUnavailable, "Upstream response was incomplete")
				}
				if usage := o.json.usage(); usage != nil {
					done.Usage = usage
				} else if o.json.has("response.usage") && o.json.value("response.usage") != "null" {
					done.Outcome = core.OutcomeIncomplete
					done.Error = gatewayError("invalid_response", core.CategoryUnavailable, "Upstream terminal usage was invalid")
				}
			}
			o.terminal = done
		}
	}
	o.data = false
	o.event = nil
	o.json = tokenObserver{}
	o.invalid = false
}
func unknownUsage() *core.UsageReport {
	return &core.UsageReport{Source: core.UsageUnknown, Completeness: core.UsageUnknownCompleteness}
}
func (o *sseObserver) complete(readErr, ctxErr error, headErr *core.GatewayError) *core.CompleteFrame {
	unknown := unknownUsage()
	if ctxErr != nil {
		return &core.CompleteFrame{Outcome: core.OutcomeCancelled, Error: transportError(ctxErr), Usage: unknown}
	}
	if headErr != nil {
		return &core.CompleteFrame{Outcome: core.OutcomeFailed, Error: headErr, Usage: unknown}
	}
	if readErr != io.EOF || o.data || o.lineLen != 0 {
		return &core.CompleteFrame{Outcome: core.OutcomeIncomplete, Error: gatewayError("incomplete_response", core.CategoryUnavailable, "Upstream response was incomplete"), Usage: unknown}
	}
	if o.terminal != nil {
		return o.terminal
	}
	return &core.CompleteFrame{Outcome: core.OutcomeIncomplete, Error: gatewayError("incomplete_response", core.CategoryUnavailable, "Upstream response had no terminal event"), Usage: unknown}
}

// tokenObserver is a bounded streaming JSON grammar checker. It keeps only
// observed scalar/key tokens; opaque strings and nested values are skipped.
type tokenObserver struct {
	stack    []jsonLevel
	mode     byte // 0 whitespace/structure, 's' string, 'n' number, 'l' literal
	escape   bool
	unicode  int
	token    []byte
	keep     bool
	keyToken bool
	keyLong  bool
	overflow bool
	bad      bool
	rootDone bool
	vals     map[string]string
	seen     map[string]bool
}
type jsonLevel struct {
	kind  byte
	state byte
	path  string
	key   string
}

func (j *tokenObserver) value(k string) string { return j.vals[k] }
func (j *tokenObserver) has(k string) bool     { return j.seen[k] }
func (j *tokenObserver) valid() bool {
	return !j.bad && !j.overflow && j.rootDone && len(j.stack) == 0 && j.mode == 0
}
func (j *tokenObserver) feed(b byte) {
	if j.bad {
		return
	}
	if j.mode == 's' {
		if j.unicode > 0 {
			if !isHex(b) {
				j.bad = true
			}
			j.unicode--
			j.add(b)
			return
		}
		if j.escape {
			j.escape = false
			if b == 'u' {
				j.unicode = 4
			} else if !strings.ContainsRune(`"\/bfnrt`, rune(b)) {
				j.bad = true
			}
			j.add(b)
			return
		}
		if b == '\\' {
			j.escape = true
			j.add(b)
			return
		}
		if b == '"' {
			j.finishString()
			return
		}
		if b < 0x20 {
			j.bad = true
		}
		j.add(b)
		return
	}
	if j.mode == 'n' || j.mode == 'l' {
		if (b >= '0' && b <= '9') || b == '.' || b == '-' || b == '+' || b == 'e' || b == 'E' || (j.mode == 'l' && b >= 'a' && b <= 'z') {
			j.add(b)
			return
		}
		j.finishScalar()
		if j.bad {
			return
		}
	}
	if b == ' ' || b == '\n' || b == '\t' || b == '\r' {
		return
	}
	if len(j.stack) == 0 {
		if j.rootDone {
			j.bad = true
			return
		}
		if b != '{' {
			j.bad = true
			return
		}
		j.stack = append(j.stack, jsonLevel{kind: '{', state: 0})
		return
	}
	top := &j.stack[len(j.stack)-1]
	if top.kind == '{' {
		switch top.state {
		case 0, 4:
			if b == '}' && top.state == 0 {
				j.pop()
				return
			}
			if b != '"' {
				j.bad = true
				return
			}
			j.startString(true)
			return
		case 1:
			if b != ':' {
				j.bad = true
				return
			}
			top.state = 2
			return
		case 2:
			j.startValue(b)
			return
		case 3:
			if b == '}' {
				j.pop()
				return
			}
			if b != ',' {
				j.bad = true
				return
			}
			top.state = 4
			return
		}
	} else {
		switch top.state {
		case 0, 2:
			if b == ']' && top.state == 0 {
				j.pop()
				return
			}
			j.startValue(b)
			return
		case 1:
			if b == ']' {
				j.pop()
				return
			}
			if b != ',' {
				j.bad = true
				return
			}
			top.state = 2
			return
		}
	}
}
func isHex(b byte) bool { return b >= '0' && b <= '9' || b >= 'a' && b <= 'f' || b >= 'A' && b <= 'F' }
func (j *tokenObserver) add(b byte) {
	if j.keep {
		if len(j.token) < scalarLimit {
			j.token = append(j.token, b)
		} else {
			if j.keyToken {
				j.keyLong = true
			} else {
				j.overflow = true
			}
		}
	}
}
func (j *tokenObserver) path() string {
	top := j.stack[len(j.stack)-1]
	if top.kind == '[' {
		return ""
	}
	if top.path == "" {
		if len(j.stack) == 1 {
			return top.key
		}
		return ""
	}
	if top.path == "response" || top.path == "response.usage" || top.path == "response.usage.input_tokens_details" || top.path == "response.usage.output_tokens_details" {
		return top.path + "." + top.key
	}
	return ""
}
func observed(p string) bool {
	switch p {
	case "type", "response", "response.status", "response.error", "response.usage", "response.usage.input_tokens", "response.usage.output_tokens", "response.usage.input_tokens_details", "response.usage.output_tokens_details", "response.usage.input_tokens_details.cached_tokens", "response.usage.output_tokens_details.reasoning_tokens":
		return true
	}
	return false
}
func (j *tokenObserver) startString(key bool) {
	j.mode = 's'
	j.token = nil
	j.escape = false
	j.unicode = 0
	j.keyToken = key
	j.keyLong = false
	j.keep = key || observed(j.path())
}
func (j *tokenObserver) startValue(b byte) {
	p := j.path()
	switch b {
	case '{', '[':
		if len(j.stack) >= 64 {
			j.bad = true
			return
		}
		if observed(p) {
			if j.seen == nil {
				j.seen = make(map[string]bool)
			}
			j.seen[p] = true
		}
		j.stack = append(j.stack, jsonLevel{kind: b, path: p})
		return
	case '"':
		j.startString(false)
		return
	default:
		j.mode = 'n'
		if b == 't' || b == 'f' || b == 'n' {
			j.mode = 'l'
		} else if b != '-' && (b < '0' || b > '9') {
			j.bad = true
			return
		}
		j.keyToken = false
		j.keep = true
		j.token = nil
		j.add(b)
	}
}
func (j *tokenObserver) finishString() {
	if j.escape || j.unicode != 0 {
		j.bad = true
		return
	}
	j.mode = 0
	top := &j.stack[len(j.stack)-1]
	if top.kind == '{' && (top.state == 0 || top.state == 4) {
		var key string
		if j.keyLong {
			key = ""
		} else if json.Unmarshal(append(append([]byte{'"'}, j.token...), '"'), &key) != nil {
			j.bad = true
			return
		}
		if key == "" {
			top.key = ""
		} else {
			top.key = key
		}
		p := j.path()
		for _, name := range []string{"type", "response", "status", "error", "usage", "input_tokens", "output_tokens", "input_tokens_details", "output_tokens_details", "cached_tokens", "reasoning_tokens"} {
			if strings.EqualFold(key, name) && key != name && (observed(p) || p == "response" || p == "response.usage" || p == "response.usage.input_tokens_details" || p == "response.usage.output_tokens_details") {
				j.bad = true
			}
		}
		if observed(p) {
			if j.seen == nil {
				j.seen = make(map[string]bool)
			}
			if j.seen[p] {
				j.bad = true
			}
			j.seen[p] = true
		}
		top.state = 1
		return
	}
	if observed(j.path()) {
		if strings.HasSuffix(j.path(), "_tokens") {
			j.bad = true
		}
		var v string
		if json.Unmarshal(append(append([]byte{'"'}, j.token...), '"'), &v) != nil {
			j.bad = true
		} else {
			j.store(v)
		}
	}
	j.endValue()
}
func (j *tokenObserver) finishScalar() {
	raw := string(j.token)
	if j.overflow || !json.Valid([]byte(raw)) {
		j.bad = true
		return
	}
	if j.mode == 'n' {
		if _, err := strconv.ParseFloat(raw, 64); err != nil {
			j.bad = true
		}
	} else if raw != "true" && raw != "false" && raw != "null" {
		j.bad = true
	}
	if !j.bad && observed(j.path()) {
		j.store(raw)
	}
	j.mode = 0
	j.endValue()
}
func (j *tokenObserver) store(v string) {
	if j.vals == nil {
		j.vals = make(map[string]string)
	}
	j.vals[j.path()] = v
}
func (j *tokenObserver) endValue() {
	top := &j.stack[len(j.stack)-1]
	if top.kind == '{' {
		top.state = 3
	} else {
		top.state = 1
	}
}
func (j *tokenObserver) pop() {
	j.stack = j.stack[:len(j.stack)-1]
	if len(j.stack) == 0 {
		j.rootDone = true
	} else {
		j.endValue()
	}
}
func (j *tokenObserver) usage() *core.UsageReport {
	if !j.has("response.usage") || j.value("response.usage") == "null" {
		return nil
	}
	input, e1 := strconv.ParseInt(j.value("response.usage.input_tokens"), 10, 64)
	output, e2 := strconv.ParseInt(j.value("response.usage.output_tokens"), 10, 64)
	if e1 != nil || e2 != nil || input < 0 || output < 0 {
		return nil
	}
	result := &core.UsageReport{InputTokens: &input, OutputTokens: &output, Source: core.UsageProvider, Completeness: core.UsageComplete}
	for _, field := range []struct {
		path string
		max  int64
		dest **int64
	}{{"response.usage.input_tokens_details.cached_tokens", input, &result.CachedTokens}, {"response.usage.output_tokens_details.reasoning_tokens", output, &result.ReasoningTokens}} {
		if j.has(field.path) {
			v, err := strconv.ParseInt(j.value(field.path), 10, 64)
			if err != nil || v < 0 || v > field.max {
				return nil
			}
			*field.dest = &v
		}
	}
	return result
}

package chatcompletions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
)

const maxEventBytes = 1 << 20

func execute(ctx context.Context, cfg config, in core.ExecutionRequest, credential string, doer core.HTTPDoer) (core.ExecutionResponse, *core.GatewayError) {
	requestCtx, cancel := context.WithCancel(ctx)
	req, e := http.NewRequestWithContext(requestCtx, http.MethodPost, cfg.Endpoint, io.NopCloser(bytes.NewReader(in.Payload.Body)))
	if e != nil {
		cancel()
		return core.ExecutionResponse{}, transportError(e)
	}
	req.GetBody = nil
	req.ContentLength = int64(len(in.Payload.Body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Accept", "application/json, text/event-stream")
	copyRequestHeaders(req.Header, in.Metadata.Headers)
	req.Header.Set("Authorization", "Bearer "+credential)
	if doer == nil {
		cancel()
		return core.ExecutionResponse{}, ge("transport_unavailable", core.CategoryUnavailable, "Runtime transport is unavailable")
	}
	resp, e := doer.Do(req)
	if e != nil {
		cancel()
		return core.ExecutionResponse{}, transportError(e)
	}
	if resp == nil || resp.Body == nil {
		cancel()
		return core.ExecutionResponse{}, ge("upstream_unavailable", core.CategoryUnavailable, "Upstream returned no response")
	}
	for _, v := range resp.Header.Values("Content-Encoding") {
		if !strings.EqualFold(strings.TrimSpace(v), "identity") {
			resp.Body.Close()
			cancel()
			return core.ExecutionResponse{}, ge("unsupported_response_encoding", core.CategoryUnavailable, "Unsupported upstream response encoding")
		}
	}
	headers := safeResponseHeaders(resp.Header)
	status := resp.StatusCode
	head := &core.HeadFrame{Protocol: protocol, ContentType: resp.Header.Get("Content-Type"), HTTPStatus: &status, Headers: headers}
	if status < 200 || status >= 300 {
		head.Error = rejection(status)
	}
	if strings.Contains(strings.ToLower(head.ContentType), "text/event-stream") {
		return core.ExecutionResponse{Stream: newSSE(resp.Body, head, cancel, cfg.StreamIdleTimeout)}, nil
	}
	return core.ExecutionResponse{Stream: newJSONStream(resp.Body, head, cancel, cfg.StreamIdleTimeout)}, nil
}

func copyRequestHeaders(dst http.Header, src map[string][]string) {
	blocked := map[string]bool{"connection": true, "keep-alive": true, "proxy-connection": true, "proxy-authenticate": true, "proxy-authorization": true, "te": true, "trailer": true, "transfer-encoding": true, "upgrade": true, "host": true, "content-length": true, "content-type": true, "accept-encoding": true, "authorization": true}
	for n, values := range src {
		if strings.EqualFold(n, "Connection") {
			for _, value := range values {
				for _, token := range strings.Split(value, ",") {
					blocked[strings.ToLower(strings.TrimSpace(token))] = true
				}
			}
		}
	}
	for n, values := range src {
		lower := strings.ToLower(n)
		if !blocked[lower] && lower != "openai-organization" && lower != "openai-project" && !strings.Contains(lower, "auth") && (lower == "idempotency-key" || !strings.Contains(lower, "key")) && !strings.Contains(lower, "secret") && !strings.Contains(lower, "token") && !strings.Contains(lower, "credential") && !strings.HasPrefix(lower, "proxy-") {
			for _, value := range values {
				dst.Add(n, value)
			}
		}
	}
}
func safeResponseHeaders(src http.Header) map[string][]string {
	blocked := map[string]bool{"connection": true, "keep-alive": true, "proxy-connection": true, "proxy-authenticate": true, "proxy-authorization": true, "te": true, "trailer": true, "transfer-encoding": true, "upgrade": true, "set-cookie": true, "authorization": true}
	for _, value := range src.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			blocked[strings.ToLower(strings.TrimSpace(token))] = true
		}
	}
	out := map[string][]string{}
	for n, values := range src {
		if !blocked[strings.ToLower(n)] {
			out[http.CanonicalHeaderKey(n)] = append([]string(nil), values...)
		}
	}
	return out
}
func unknown() *core.UsageReport {
	return &core.UsageReport{Source: core.UsageUnknown, Completeness: core.UsageUnknownCompleteness}
}
func jsonUsage(body []byte) *core.UsageReport {
	fields, ok := distinct(body, "usage")
	if !ok || fields["usage"] == nil || string(fields["usage"]) == "null" {
		return unknown()
	}
	u := fields["usage"]
	top, ok := distinct(u, "prompt_tokens", "completion_tokens", "prompt_tokens_details", "completion_tokens_details")
	if !ok {
		return unknown()
	}
	var counts struct {
		In  *int64 `json:"prompt_tokens"`
		Out *int64 `json:"completion_tokens"`
	}
	if json.Unmarshal(u, &counts) != nil || counts.In == nil || counts.Out == nil || *counts.In < 0 || *counts.Out < 0 {
		return unknown()
	}
	r := &core.UsageReport{InputTokens: counts.In, OutputTokens: counts.Out, Source: core.UsageProvider, Completeness: core.UsageComplete}
	if raw := top["prompt_tokens_details"]; raw != nil && string(raw) != "null" {
		_, ok := distinct(raw, "cached_tokens")
		if !ok {
			return unknown()
		}
		var x struct {
			N *int64 `json:"cached_tokens"`
		}
		if json.Unmarshal(raw, &x) != nil || x.N != nil && (*x.N < 0 || *x.N > *counts.In) {
			return unknown()
		}
		r.CachedTokens = x.N
	}
	if raw := top["completion_tokens_details"]; raw != nil && string(raw) != "null" {
		_, ok := distinct(raw, "reasoning_tokens")
		if !ok {
			return unknown()
		}
		var x struct {
			N *int64 `json:"reasoning_tokens"`
		}
		if json.Unmarshal(raw, &x) != nil || x.N != nil && (*x.N < 0 || *x.N > *counts.Out) {
			return unknown()
		}
		r.ReasoningTokens = x.N
	}
	return r
}
func distinct(raw []byte, names ...string) (map[string]json.RawMessage, bool) {
	d := json.NewDecoder(bytes.NewReader(raw))
	t, e := d.Token()
	if e != nil || t != json.Delim('{') {
		return nil, false
	}
	out := map[string]json.RawMessage{}
	for d.More() {
		kt, e := d.Token()
		if e != nil {
			return nil, false
		}
		k, ok := kt.(string)
		if !ok {
			return nil, false
		}
		var v json.RawMessage
		if d.Decode(&v) != nil {
			return nil, false
		}
		for _, n := range names {
			if strings.EqualFold(k, n) {
				if k != n || out[n] != nil {
					return nil, false
				}
				out[n] = v
			}
		}
	}
	_, e = d.Token()
	return out, e == nil
}

func rejection(status int) *core.GatewayError {
	cat := core.CategoryUnavailable
	switch status {
	case 400, 422:
		cat = core.CategoryInvalidRequest
	case 401:
		cat = core.CategoryUnauthenticated
	case 403:
		cat = core.CategoryPermissionDenied
	case 429:
		cat = core.CategoryRateLimited
	}
	g := ge("upstream_rejection", cat, fmt.Sprintf("Upstream rejected request (HTTP %d)", status))
	g.RetryDisposition = core.RetryUnsafe
	return g
}
func transportError(e error) *core.GatewayError {
	if errors.Is(e, context.Canceled) {
		return ge("upstream_cancelled", core.CategoryCancelled, "Upstream request cancelled")
	}
	if errors.Is(e, context.DeadlineExceeded) {
		return ge("upstream_timeout", core.CategoryTimeout, "Upstream request timed out")
	}
	var n net.Error
	if errors.As(e, &n) && n.Timeout() {
		return ge("upstream_timeout", core.CategoryTimeout, "Upstream request timed out")
	}
	return ge("upstream_unavailable", core.CategoryUnavailable, "Upstream request failed")
}

type sseStream struct {
	mu          sync.Mutex
	body        io.ReadCloser
	head        *core.HeadFrame
	phase       int
	pending     error
	observer    observer
	cancel      context.CancelFunc
	idleTimeout time.Duration
	idleExpired atomic.Bool
}

func newSSE(body io.ReadCloser, head *core.HeadFrame, cancel context.CancelFunc, idleTimeout time.Duration) *sseStream {
	return &sseStream{body: body, head: head, cancel: cancel, idleTimeout: idleTimeout}
}
func (s *sseStream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase == 3 {
		return nil
	}
	s.phase = 3
	err := s.body.Close()
	s.cancel()
	return err
}
func (s *sseStream) Next(ctx context.Context) (core.StreamFrame, error) {
	if err := ctx.Err(); err != nil {
		_ = s.Close()
		return core.StreamFrame{}, err
	}
	s.mu.Lock()
	if s.phase == 0 {
		s.phase = 1
		h := s.head
		s.mu.Unlock()
		return core.StreamFrame{Type: core.FrameHead, Head: h}, nil
	}
	if s.phase >= 2 {
		s.mu.Unlock()
		return core.StreamFrame{}, io.EOF
	}
	s.mu.Unlock()
	stop := context.AfterFunc(ctx, func() { _ = s.Close() })
	defer stop()
	buf := make([]byte, 4096)
	var n int
	var e error
	if s.pending != nil {
		e, s.pending = s.pending, nil
	} else {
		var timer *time.Timer
		var timerDone chan struct{}
		if s.idleTimeout > 0 {
			timerDone = make(chan struct{})
			timer = time.AfterFunc(s.idleTimeout, func() {
				defer close(timerDone)
				s.idleExpired.Store(true)
				_ = s.body.Close()
				s.cancel()
			})
		}
		n, e = s.body.Read(buf)
		if timer != nil && !timer.Stop() {
			<-timerDone
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase == 3 {
		return core.StreamFrame{}, context.Canceled
	}
	if ctx.Err() != nil {
		return core.StreamFrame{}, ctx.Err()
	}
	if s.idleExpired.Load() {
		s.phase = 2
		done := &core.CompleteFrame{Outcome: core.OutcomeIncomplete, Error: ge("upstream_timeout", core.CategoryTimeout, "Upstream stream idle timeout"), Usage: unknown()}
		s.cancel()
		return core.StreamFrame{Type: core.FrameComplete, Complete: done}, nil
	}
	if n > 0 {
		s.pending = e
		s.observer.feed(buf[:n])
		return core.StreamFrame{Type: core.FrameBody, Body: &core.BodyFrame{Data: buf[:n]}}, nil
	}
	s.phase = 2
	_ = s.body.Close()
	s.cancel()
	done := s.observer.complete(e, s.head.Error)
	return core.StreamFrame{Type: core.FrameComplete, Complete: done}, nil
}

type observer struct {
	event        []byte
	line         []byte
	cr           bool
	bad          bool
	usage        *core.UsageReport
	terminal     bool
	postTerminal bool
}

func (o *observer) feed(p []byte) {
	for _, b := range p {
		if o.terminal {
			o.postTerminal = true
		}
		if b == '\n' || b == '\r' {
			if o.cr && b == '\n' {
				o.cr = false
				continue
			}
			o.cr = b == '\r'
			o.endLine()
			continue
		}
		o.cr = false
		if len(o.event)+len(o.line) >= maxEventBytes {
			o.bad = true
		} else {
			o.line = append(o.line, b)
		}
	}
}
func (o *observer) endLine() {
	if len(o.line) == 0 {
		o.endEvent()
		return
	}
	o.event = append(o.event, o.line...)
	o.event = append(o.event, '\n')
	o.line = nil
}
func (o *observer) endEvent() {
	if len(o.event) == 0 {
		return
	}
	var data []byte
	for _, line := range bytes.Split(o.event, []byte{'\n'}) {
		if bytes.HasPrefix(line, []byte("data:")) {
			v := bytes.TrimPrefix(line, []byte("data:"))
			if len(v) > 0 && v[0] == ' ' {
				v = v[1:]
			}
			if data != nil {
				data = append(data, '\n')
			}
			data = append(data, v...)
		}
	}
	if len(data) > 0 {
		if bytes.Equal(data, []byte("[DONE]")) {
			o.terminal = true
		} else {
			if !json.Valid(data) || !unique(data) {
				o.bad = true
			} else {
				var x map[string]json.RawMessage
				if json.Unmarshal(data, &x) != nil || x == nil {
					o.bad = true
				} else if rawChoices, ok := x["choices"]; ok && (len(rawChoices) == 0 || rawChoices[0] != '[') {
					o.bad = true
				} else if rawChoices, ok := x["choices"]; ok && choicesEmpty(rawChoices) && len(x["usage"]) > 0 {
					wrapped := append(append([]byte(`{"usage":`), x["usage"]...), '}')
					u := jsonUsage(wrapped)
					if u.Source == core.UsageProvider {
						o.usage = u
					}
				}
			}
		}
	}
	o.event = nil
}
func choicesEmpty(raw json.RawMessage) bool {
	var choices []json.RawMessage
	return json.Unmarshal(raw, &choices) == nil && choices != nil && len(choices) == 0
}
func (o *observer) complete(e error, headErr *core.GatewayError) *core.CompleteFrame {
	u := o.usage
	if u == nil {
		u = unknown()
	}
	if headErr != nil {
		return &core.CompleteFrame{Outcome: core.OutcomeFailed, Error: headErr, Usage: u}
	}
	if e != io.EOF || len(o.event) > 0 || len(o.line) > 0 || o.bad || !o.terminal || o.postTerminal {
		return &core.CompleteFrame{Outcome: core.OutcomeIncomplete, Error: ge("incomplete_response", core.CategoryUnavailable, "Upstream stream was malformed or incomplete"), Usage: u}
	}
	return &core.CompleteFrame{Outcome: core.OutcomeSucceeded, Usage: u}
}

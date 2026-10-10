package codex

import (
	"context"
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

const (
	executeChunkSize      = 4 << 10
	maxCodexResponseBytes = 64 << 20
	maxCodexErrorBytes    = 64 << 10
)

func codexResponseHead(resp *http.Response) *core.HeadFrame {
	headers := resp.Header.Clone()
	blocked := map[string]bool{
		"connection": true, "keep-alive": true, "proxy-authenticate": true, "proxy-authorization": true,
		"te": true, "trailer": true, "transfer-encoding": true, "upgrade": true,
		"set-cookie": true, "set-cookie2": true, "cookie": true, "cookie2": true,
		"authorization": true, "proxy-connection": true, "x-api-key": true,
		"chatgpt-account-id": true, "openai-organization": true, "openai-project": true,
		"location": true, "www-authenticate": true,
	}
	for _, value := range headers.Values("Connection") {
		for _, field := range strings.Split(value, ",") {
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
		head.Error = connectorError("upstream_rejection", core.CategoryUnavailable, fmt.Sprintf("Upstream rejected request (HTTP %d)", status))
		switch status {
		case http.StatusBadRequest, http.StatusUnprocessableEntity:
			head.Error.Category = core.CategoryInvalidRequest
		case http.StatusUnauthorized:
			head.Error.Category = core.CategoryUnauthenticated
		case http.StatusForbidden:
			head.Error.Category = core.CategoryPermissionDenied
		case http.StatusTooManyRequests:
			head.Error.Category = core.CategoryRateLimited
		}
	}
	return head
}

type executeStream struct {
	mu             sync.Mutex
	body           io.ReadCloser
	cancel         context.CancelFunc
	head           *core.HeadFrame
	phase          int
	readBytes      int64
	pending        error
	tooLarge       bool
	idleTimeout    time.Duration
	responseLimit  int64
	errorLimit     int64
	idleExpired    atomic.Bool
	cleanupOnce    sync.Once
	cleanupErr     error
	closeTransport func()
	stopRequest    func() bool
	observer       sseObserver
}

func (s *executeStream) Close() error {
	s.mu.Lock()
	if s.phase != 3 {
		s.phase = 3
	}
	s.mu.Unlock()
	if s.stopRequest != nil {
		s.stopRequest()
	}
	s.cleanup()
	return s.cleanupErr
}

func (s *executeStream) abort() {
	s.mu.Lock()
	if s.phase < 2 && !s.idleExpired.Load() {
		s.phase = 3
	}
	s.mu.Unlock()
	s.cleanup()
}

func (s *executeStream) cleanup() {
	s.cleanupOnce.Do(func() {
		s.cancel()
		s.cleanupErr = s.body.Close()
		if s.closeTransport != nil {
			s.closeTransport()
		}
	})
}

func (s *executeStream) Next(ctx context.Context) (core.StreamFrame, error) {
	if err := ctx.Err(); err != nil {
		_ = s.Close()
		return core.StreamFrame{}, err
	}
	s.mu.Lock()
	switch s.phase {
	case 0:
		s.phase = 1
		head := s.head
		s.mu.Unlock()
		return core.StreamFrame{Type: core.FrameHead, Head: head}, nil
	case 2:
		s.mu.Unlock()
		return core.StreamFrame{}, io.EOF
	case 3:
		s.mu.Unlock()
		return core.StreamFrame{}, context.Canceled
	}
	pending, tooLarge := s.pending, s.tooLarge
	s.mu.Unlock()
	stop := context.AfterFunc(ctx, func() { _ = s.Close() })
	defer stop()
	var n int
	var err error
	var readBuf []byte
	if !tooLarge && pending == nil {
		limit := s.responseLimit
		if limit <= 0 {
			limit = maxCodexResponseBytes
		}
		if s.head.Error != nil {
			limit = s.errorLimit
			if limit <= 0 {
				limit = maxCodexErrorBytes
			}
		}
		remaining := limit - s.readBytes
		if remaining < 0 {
			remaining = 0
		}
		readBuf = make([]byte, executeChunkSize)
		if remaining < int64(len(readBuf)) {
			readBuf = readBuf[:remaining+1]
		}
		var timer *time.Timer
		var timerDone chan struct{}
		if s.idleTimeout > 0 {
			timerDone = make(chan struct{})
			timer = time.AfterFunc(s.idleTimeout, func() {
				defer close(timerDone)
				s.mu.Lock()
				active := s.phase == 1
				if active {
					s.idleExpired.Store(true)
				}
				s.mu.Unlock()
				if active {
					s.cleanup()
				}
			})
		}
		for n == 0 && err == nil && ctx.Err() == nil {
			n, err = s.body.Read(readBuf)
		}
		if timer != nil && !timer.Stop() {
			<-timerDone
		}
		if int64(n) > remaining {
			n = int(remaining)
			s.tooLarge = true
		}
	} else if pending != nil {
		err = pending
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx.Err() != nil {
		return core.StreamFrame{}, ctx.Err()
	}
	if s.idleExpired.Load() {
		return s.finish(core.OutcomeIncomplete, connectorError("upstream_timeout", core.CategoryTimeout, "Upstream response body idle timeout")), nil
	}
	if s.phase == 3 {
		return core.StreamFrame{}, context.Canceled
	}
	if s.head.Error == nil && s.observer.err != nil {
		return s.finish(core.OutcomeIncomplete, connectorError("invalid_sse", core.CategoryUnavailable, "Upstream response contained invalid SSE framing")), nil
	}
	if n > 0 {
		if tooLarge {
			s.tooLarge = true
		}
		s.readBytes += int64(n)
		s.pending = err
		if s.head.Error == nil {
			s.observer.feed(readBuf[:n])
		}
		return s.bodyFrame(readBuf[:n]), nil
	}
	if s.tooLarge {
		return s.finish(core.OutcomeIncomplete, connectorError("response_too_large", core.CategoryUnavailable, "Upstream response exceeded the profile limit")), nil
	}
	if s.head.Error != nil {
		return s.finish(core.OutcomeFailed, s.head.Error), nil
	}
	if err != io.EOF {
		return s.finish(core.OutcomeIncomplete, codexTransportError(err)), nil
	}
	if s.head.Error == nil && s.observer.finish() != nil {
		return s.finish(core.OutcomeIncomplete, connectorError("invalid_sse", core.CategoryUnavailable, "Upstream response contained invalid SSE framing")), nil
	}
	if s.observer.terminal != nil {
		terminal := s.observer.terminal
		return s.finish(terminal.Outcome, terminal.Error), nil
	}
	return s.finish(core.OutcomeIncomplete, connectorError("incomplete_response", core.CategoryUnavailable, "Upstream response ended without terminal SSE verification")), nil
}

func (s *executeStream) bodyFrame(data []byte) core.StreamFrame {
	return core.StreamFrame{Type: core.FrameBody, Body: &core.BodyFrame{Data: data}}
}

func (s *executeStream) finish(outcome core.Outcome, failure *core.GatewayError) core.StreamFrame {
	s.phase = 2
	if s.stopRequest != nil {
		s.stopRequest()
	}
	s.cleanup()
	return core.StreamFrame{Type: core.FrameComplete, Complete: &core.CompleteFrame{Outcome: outcome, Error: failure, Usage: s.observer.finalUsage(outcome)}}
}

func codexTransportError(err error) *core.GatewayError {
	if errors.Is(err, context.Canceled) {
		return connectorError("upstream_cancelled", core.CategoryCancelled, "Upstream request cancelled")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return connectorError("upstream_timeout", core.CategoryTimeout, "Upstream request timed out")
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return connectorError("upstream_timeout", core.CategoryTimeout, "Upstream request timed out")
	}
	return connectorError("upstream_unavailable", core.CategoryUnavailable, "Upstream request failed")
}

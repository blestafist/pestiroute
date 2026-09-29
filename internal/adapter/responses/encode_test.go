package responses

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
)

type encodeStream struct {
	frames []core.StreamFrame
	err    error
	closed bool
	next   func(context.Context) (core.StreamFrame, error)
}

func (s *encodeStream) Next(ctx context.Context) (core.StreamFrame, error) {
	if s.next != nil {
		return s.next(ctx)
	}
	if len(s.frames) == 0 {
		if s.err != nil {
			return core.StreamFrame{}, s.err
		}
		return core.StreamFrame{}, io.EOF
	}
	f := s.frames[0]
	s.frames = s.frames[1:]
	return f, nil
}
func (s *encodeStream) Close() error { s.closed = true; return nil }

func TestEncodeOpaqueCommitAndFailure(t *testing.T) {
	status := http.StatusTooManyRequests
	data := []byte{'d', 'a', 't', 'a', ':', ' ', 0xff, '\r', '\n', '\r', '\n'}
	for _, failure := range []error{nil, errors.New("upstream dropped")} {
		s := &encodeStream{frames: []core.StreamFrame{
			{Type: core.FrameHead, Head: &core.HeadFrame{HTTPStatus: &status, ContentType: "text/event-stream", Error: &core.GatewayError{Provider: "private", OriginalError: "secret"}, Headers: map[string][]string{"Connection": {"X-Hidden"}, "X-Hidden": {"secret"}, "Transfer-Encoding": {"chunked"}, "X-Safe": {"one", "two"}}}},
			{Type: core.FrameBody, Body: &core.BodyFrame{Data: data[:4]}},
			{Type: core.FrameBody, Body: &core.BodyFrame{Data: data[4:]}},
		}, err: failure}
		if failure == nil {
			s.frames = append(s.frames, core.StreamFrame{Type: core.FrameComplete, Complete: &core.CompleteFrame{Outcome: core.OutcomeFailed, Error: &core.GatewayError{Message: "never write me"}}})
		}
		w := httptest.NewRecorder()
		err := Encode(w, httptest.NewRequest(http.MethodPost, "/v1/responses", nil), core.ExecutionResponse{Stream: s}, nil)
		if failure == nil && err != nil || failure != nil && !errors.Is(err, failure) {
			t.Fatalf("unexpected error: %v", err)
		}
		if !s.closed || w.Code != status || !bytes.Equal(w.Body.Bytes(), data) {
			t.Fatalf("commit/body/close: status %d body %q closed %t", w.Code, w.Body.Bytes(), s.closed)
		}
		if w.Header().Get("X-Hidden") != "" || w.Header().Get("Transfer-Encoding") != "" || len(w.Header().Values("X-Safe")) != 2 || w.Header().Get("Content-Type") != "text/event-stream" {
			t.Fatalf("unsafe or missing headers: %v", w.Header())
		}
	}
}

func TestEncodeGatewayErrors(t *testing.T) {
	for category, status := range map[core.ErrorCategory]int{
		core.CategoryInvalidRequest: 400, core.CategoryUnsupportedFeature: 400, core.CategoryUnauthenticated: 401,
		core.CategoryPermissionDenied: 403, core.CategoryRateLimited: 429, core.CategoryUnavailable: 503,
		core.CategoryTimeout: 504, core.CategoryCancelled: 500, core.CategoryInternal: 500, "other": 500,
	} {
		t.Run(string(category), func(t *testing.T) {
			delay := 1500 * time.Millisecond
			s := &encodeStream{}
			w := httptest.NewRecorder()
			err := Encode(w, nil, core.ExecutionResponse{Stream: s}, &core.GatewayError{Category: category, Code: "gateway_code", Message: "safe", Provider: "private", OriginalError: "secret", RetryAfter: &delay})
			if err != nil || !s.closed || w.Code != status || w.Header().Get("Retry-After") != "2" || w.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("error response: err %v status %d headers %v", err, w.Code, w.Header())
			}
			want := `{"error":{"message":"safe","type":"` + string(category) + `","code":"gateway_code"}}`
			if w.Body.String() != want {
				t.Fatalf("body %q want %q", w.Body.String(), want)
			}
		})
	}
}

type failWriter struct {
	header http.Header
	status int
	writes int
	short  bool
}

func (w *failWriter) Header() http.Header    { return w.header }
func (w *failWriter) WriteHeader(status int) { w.status = status }
func (w *failWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.short {
		return 0, nil
	}
	return 0, io.ErrClosedPipe
}

func TestEncodeWriterFailureClosesStream(t *testing.T) {
	for _, short := range []bool{false, true} {
		status := 200
		s := &encodeStream{frames: []core.StreamFrame{{Type: core.FrameHead, Head: &core.HeadFrame{HTTPStatus: &status}}, {Type: core.FrameBody, Body: &core.BodyFrame{Data: []byte("raw")}}}}
		w := &failWriter{header: make(http.Header), short: short}
		err := Encode(w, httptest.NewRequest("POST", "/v1/responses", nil), core.ExecutionResponse{Stream: s}, nil)
		if short && !errors.Is(err, io.ErrShortWrite) || !short && !errors.Is(err, io.ErrClosedPipe) || !s.closed || w.status != 200 || w.writes != 1 || len(s.frames) != 0 {
			t.Fatalf("failure cleanup: %v %#v %#v", err, s, w)
		}
	}
}

func TestEncodeCancellationClosesBlockedStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	s := &encodeStream{next: func(ctx context.Context) (core.StreamFrame, error) {
		close(started)
		<-ctx.Done()
		return core.StreamFrame{}, ctx.Err()
	}}
	done := make(chan error, 1)
	go func() {
		done <- Encode(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/responses", nil).WithContext(ctx), core.ExecutionResponse{Stream: s}, nil)
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || !s.closed {
			t.Fatalf("cancellation: %v closed %t", err, s.closed)
		}
	case <-time.After(time.Second):
		t.Fatal("encoder did not stop after cancellation")
	}
}

type signalWriter struct {
	*httptest.ResponseRecorder
	written chan struct{}
}

func (w *signalWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(p)
	select {
	case w.written <- struct{}{}:
	default:
	}
	return n, err
}

func TestEncodeWritesBeforeStreamCompletes(t *testing.T) {
	status := 200
	gate := make(chan struct{})
	first := []byte("data: first\n\n")
	count := 0
	s := &encodeStream{next: func(ctx context.Context) (core.StreamFrame, error) {
		count++
		switch count {
		case 1:
			return core.StreamFrame{Type: core.FrameHead, Head: &core.HeadFrame{HTTPStatus: &status}}, nil
		case 2:
			return core.StreamFrame{Type: core.FrameBody, Body: &core.BodyFrame{Data: first}}, nil
		case 3:
			select {
			case <-gate:
				return core.StreamFrame{Type: core.FrameComplete, Complete: &core.CompleteFrame{Outcome: core.OutcomeSucceeded}}, nil
			case <-ctx.Done():
				return core.StreamFrame{}, ctx.Err()
			}
		default:
			return core.StreamFrame{}, io.EOF
		}
	}}
	w := &signalWriter{ResponseRecorder: httptest.NewRecorder(), written: make(chan struct{}, 1)}
	done := make(chan error, 1)
	go func() {
		done <- Encode(w, httptest.NewRequest("POST", "/v1/responses", nil), core.ExecutionResponse{Stream: s}, nil)
	}()
	select {
	case <-w.written:
		// The stream cannot complete until gate opens: Body reached the writer early.
	case <-time.After(time.Second):
		close(gate)
		<-done
		t.Fatal("body was buffered until completion")
	}
	close(gate)
	if err := <-done; err != nil || !s.closed || !bytes.Equal(w.Body.Bytes(), first) {
		t.Fatalf("stream result: %v closed %t body %q", err, s.closed, w.Body.Bytes())
	}
}

func TestEncodePreHeadFailureDoesNotInventResponse(t *testing.T) {
	s := &encodeStream{err: errors.New("pre-head failure")}
	w := httptest.NewRecorder()
	err := Encode(w, httptest.NewRequest("POST", "/v1/responses", nil), core.ExecutionResponse{Stream: s}, nil)
	if err == nil || !strings.Contains(err.Error(), "pre-head failure") || !s.closed || w.Body.Len() != 0 || w.Code != 200 {
		t.Fatalf("pre-head: %v %#v", err, w)
	}
}

func TestEncodeRejectsInformationalHead(t *testing.T) {
	for _, status := range []int{100, 103, 199} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			s := &encodeStream{frames: []core.StreamFrame{{Type: core.FrameHead, Head: &core.HeadFrame{HTTPStatus: &status}}}}
			w := &failWriter{header: make(http.Header)}
			err := Encode(w, httptest.NewRequest("POST", "/v1/responses", nil), core.ExecutionResponse{Stream: s}, nil)
			if !errors.Is(err, core.ErrStreamContract) || !s.closed || w.status != 0 || w.writes != 0 || len(s.frames) != 0 {
				t.Fatalf("informational head: err %v closed %t status %d writes %d remaining %d", err, s.closed, w.status, w.writes, len(s.frames))
			}
		})
	}
}

func TestEncodeRejectsInvalidHeadOrder(t *testing.T) {
	status := 201
	for name, frames := range map[string][]core.StreamFrame{
		"body before head": {{Type: core.FrameBody, Body: &core.BodyFrame{Data: []byte("early")}}},
		"duplicate head":   {{Type: core.FrameHead, Head: &core.HeadFrame{HTTPStatus: &status}}, {Type: core.FrameHead, Head: &core.HeadFrame{HTTPStatus: &status}}},
	} {
		t.Run(name, func(t *testing.T) {
			s := &encodeStream{frames: frames}
			w := &failWriter{header: make(http.Header)}
			err := Encode(w, httptest.NewRequest("POST", "/v1/responses", nil), core.ExecutionResponse{Stream: s}, nil)
			wantStatus := 0
			if name == "duplicate head" {
				wantStatus = status
			}
			if !errors.Is(err, core.ErrStreamContract) || !s.closed || w.status != wantStatus || w.writes != 0 {
				t.Fatalf("head order: err %v closed %t status %d writes %d", err, s.closed, w.status, w.writes)
			}
		})
	}
}

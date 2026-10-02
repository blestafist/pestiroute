// Package fakeupstream provides a controllable loopback HTTP upstream for tests.
package fakeupstream

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
)

// Request is a snapshot of the incoming request. Cancelled closes when the
// client disconnects while the handler is still running.
type Request struct {
	Method, Path, RawQuery string
	Header                 http.Header
	Body                   []byte
	Cancelled              <-chan struct{}
	Completed              <-chan struct{}
	CancelCount            func() int64
}

// Step is one stream write. Closing Gate permits the write; Sent closes after
// the write has been flushed. Drop closes the connection after this step.
type Step struct {
	Gate    <-chan struct{}
	Waiting chan<- struct{} // Notified when the handler reaches Gate.
	Sent    chan<- struct{}
	Data    []byte
	Drop    bool
}

// Response describes a fixed response or a sequence of independently gated
// stream writes. HeaderGate delays headers; Drop closes the connection before
// response headers are sent.
type Response struct {
	Status     int
	Header     http.Header
	Body       []byte
	Steps      []Step
	Drop       bool
	HeaderGate <-chan struct{}
}

// Server is a loopback-only test server. Read Requests as each call arrives;
// its one-slot queue intentionally bounds unconsumed captures.
type Server struct {
	*httptest.Server
	Requests <-chan Request
	requests atomic.Int64
}

func (s *Server) RequestCount() int64 { return s.requests.Load() }

func New(response Response) *Server {
	server := &Server{}
	requests := make(chan Request, 1)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		completed := make(chan struct{})
		defer close(completed)
		server.requests.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return
		}
		cancelled := make(chan struct{})
		done := make(chan struct{})
		watcherDone := make(chan struct{})
		var once sync.Once
		var cancelCount atomic.Int64
		signal := func() { once.Do(func() { cancelCount.Add(1); close(cancelled) }) }
		go func() {
			defer close(watcherDone)
			select {
			case <-r.Context().Done():
				select {
				case <-done:
				default:
					signal()
				}
			case <-done:
			}
		}()
		defer func() {
			close(done)
			<-watcherDone
		}()

		capture := Request{Method: r.Method, Path: r.URL.Path, RawQuery: r.URL.RawQuery,
			Header: r.Header.Clone(), Body: body, Cancelled: cancelled, Completed: completed, CancelCount: cancelCount.Load}
		select {
		case requests <- capture:
		case <-r.Context().Done():
			return
		}
		if response.Drop {
			drop(w)
			return
		}
		if response.HeaderGate != nil {
			select {
			case <-response.HeaderGate:
			case <-r.Context().Done():
				signal()
				return
			}
		}
		for key, values := range response.Header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		if len(response.Steps) == 0 {
			if response.Status != 0 {
				w.WriteHeader(response.Status)
			}
			_, _ = w.Write(response.Body)
			return
		}
		if response.Header.Get("Content-Type") == "" {
			w.Header().Set("Content-Type", "text/event-stream")
		}
		if response.Status != 0 {
			w.WriteHeader(response.Status)
		}
		w.(http.Flusher).Flush()
		for _, step := range response.Steps {
			if step.Gate != nil {
				if step.Waiting != nil {
					select {
					case step.Waiting <- struct{}{}:
					default:
					}
				}
				select {
				case <-step.Gate:
				case <-r.Context().Done():
					signal()
					return
				}
			}
			if _, err := w.Write(step.Data); err != nil {
				if r.Context().Err() != nil {
					signal()
				}
				return
			}
			w.(http.Flusher).Flush()
			if step.Sent != nil {
				close(step.Sent)
			}
			if step.Drop {
				drop(w)
				return
			}
		}
	}))
	server.Server = s
	server.Requests = requests
	return server
}

func drop(w http.ResponseWriter) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		_ = conn.Close()
	}
}

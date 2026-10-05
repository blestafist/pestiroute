package codex

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
)

type countedBody struct {
	reader *bytes.Reader
	reads  atomic.Int32
	closes atomic.Int32
}

func (b *countedBody) Read(p []byte) (int, error) {
	b.reads.Add(1)
	return b.reader.Read(p)
}
func (b *countedBody) Close() error { b.closes.Add(1); return nil }

func newTestExecuteStream(body io.ReadCloser, status int) *executeStream {
	head := &core.HeadFrame{Protocol: protocol, HTTPStatus: &status}
	if status < 200 || status >= 300 {
		head.Error = connectorError("upstream_rejection", core.CategoryUnavailable, "rejected")
	}
	_, cancel := context.WithCancel(context.Background())
	return &executeStream{body: body, cancel: cancel, head: head}
}

func drainExecuteStream(t *testing.T, s *executeStream) ([]byte, *core.CompleteFrame) {
	t.Helper()
	var out []byte
	if frame, err := s.Next(t.Context()); err != nil || frame.Type != core.FrameHead {
		t.Fatalf("Head=%+v err=%v", frame, err)
	}
	for {
		frame, err := s.Next(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if frame.Type == core.FrameComplete {
			return out, frame.Complete
		}
		if frame.Type != core.FrameBody {
			t.Fatalf("unexpected frame %+v", frame)
		}
		out = append(out, frame.Body.Data...)
	}
}

func TestExecuteLimitsAreBounded(t *testing.T) {
	if maxCodexResponseBytes != 64<<20 || maxCodexErrorBytes != 64<<10 {
		t.Fatalf("normative limits = %d / %d", maxCodexResponseBytes, maxCodexErrorBytes)
	}
	for _, tc := range []struct {
		name   string
		status int
		limit  int64
		data   string
	}{
		{"success response", http.StatusOK, 4, "12345"},
		{"rejection diagnostic", http.StatusUnauthorized, 3, "1234"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &countedBody{reader: bytes.NewReader([]byte(tc.data))}
			s := newTestExecuteStream(body, tc.status)
			if tc.status == http.StatusOK {
				s.responseLimit = tc.limit
			} else {
				s.errorLimit = tc.limit
			}
			output, complete := drainExecuteStream(t, s)
			if int64(len(output)) != tc.limit || complete.Outcome != core.OutcomeIncomplete || complete.Error == nil || complete.Error.Code != "response_too_large" {
				t.Fatalf("delivered=%d completion=%+v", len(output), complete)
			}
			if body.closes.Load() != 1 {
				t.Fatalf("body close count=%d", body.closes.Load())
			}
		})
	}
}

func TestExecuteStreamReadsOnlyForConsumer(t *testing.T) {
	body := &countedBody{reader: bytes.NewReader([]byte(strings.Repeat("x", executeChunkSize*3)))}
	s := newTestExecuteStream(body, http.StatusOK)
	if _, err := s.Next(t.Context()); err != nil {
		t.Fatal(err)
	}
	if body.reads.Load() != 0 {
		t.Fatalf("read before downstream requested Body: %d", body.reads.Load())
	}
	frame, err := s.Next(t.Context())
	if err != nil || frame.Type != core.FrameBody || len(frame.Body.Data) != executeChunkSize || body.reads.Load() != 1 {
		t.Fatalf("frame=%+v err=%v reads=%d", frame, err, body.reads.Load())
	}
	if body.reads.Load() != 1 {
		t.Fatalf("read ahead while consumer paused: %d", body.reads.Load())
	}
	_ = s.Close()
}

func TestCodexResponseHeadFiltersSensitiveAndConnectionHeaders(t *testing.T) {
	head := codexResponseHead(&http.Response{StatusCode: http.StatusOK, Header: http.Header{
		"X-Request-Id": {"safe"}, "Authorization": {"bearer-secret"}, "Set-Cookie": {"session=secret"},
		"X-Api-Key": {"key-secret"}, "ChatGPT-Account-Id": {"account-secret"},
		"Location":   {"https://private.example/redirect"},
		"Connection": {"X-Internal"}, "X-Internal": {"private"},
	}})
	for _, name := range []string{"Authorization", "Set-Cookie", "X-Api-Key", "ChatGPT-Account-Id", "Location", "X-Internal"} {
		if got := http.Header(head.Headers).Get(name); got != "" {
			t.Errorf("sensitive response header %s forwarded: %q", name, got)
		}
	}
	if got := http.Header(head.Headers).Get("X-Request-Id"); got != "safe" {
		t.Fatalf("safe request ID removed: %q", got)
	}
}

type blockingCloseBody struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	closes  atomic.Int32
}

func (b *blockingCloseBody) Read([]byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	<-b.release
	return 0, io.ErrClosedPipe
}
func (b *blockingCloseBody) Close() error {
	b.closes.Add(1)
	select {
	case <-b.release:
	default:
		close(b.release)
	}
	return nil
}

func TestExecuteStreamConcurrentCloseAndNextCleanupOnce(t *testing.T) {
	body := &blockingCloseBody{started: make(chan struct{}), release: make(chan struct{})}
	s := newTestExecuteStream(body, http.StatusOK)
	var transportCloses atomic.Int32
	s.closeTransport = func() { transportCloses.Add(1) }
	if _, err := s.Next(t.Context()); err != nil {
		t.Fatal(err)
	}
	nextDone := make(chan error, 1)
	go func() { _, err := s.Next(context.Background()); nextDone <- err }()
	select {
	case <-body.started:
	case <-time.After(time.Second):
		t.Fatal("Next did not block in reader")
	}
	var closes sync.WaitGroup
	for range 8 {
		closes.Add(1)
		go func() { defer closes.Done(); _ = s.Close() }()
	}
	closes.Wait()
	select {
	case err := <-nextDone:
		if err == nil {
			t.Fatal("concurrent Next unexpectedly succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not interrupt Next")
	}
	if body.closes.Load() != 1 || transportCloses.Load() != 1 {
		t.Fatalf("body closes=%d transport closes=%d", body.closes.Load(), transportCloses.Load())
	}
}

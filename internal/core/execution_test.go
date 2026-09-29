package core

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
)

type scriptedStream struct {
	frames  []StreamFrame
	err     error
	closed  atomic.Int32
	block   bool
	entered chan struct{}
	release chan struct{}
}

func (s *scriptedStream) Next(ctx context.Context) (StreamFrame, error) {
	if s.block {
		if s.entered != nil {
			close(s.entered)
		}
		if s.release != nil {
			select {
			case <-s.release:
				return StreamFrame{}, io.EOF
			case <-ctx.Done():
				return StreamFrame{}, ctx.Err()
			}
		}
		<-ctx.Done()
		return StreamFrame{}, ctx.Err()
	}
	if len(s.frames) == 0 {
		if s.err != nil {
			return StreamFrame{}, s.err
		}
		return StreamFrame{}, io.EOF
	}
	f := s.frames[0]
	s.frames = s.frames[1:]
	return f, nil
}

func (s *scriptedStream) Close() error {
	if s.closed.Add(1) == 1 && s.release != nil {
		close(s.release)
	}
	return nil
}

func head() StreamFrame {
	return StreamFrame{Type: FrameHead, Head: &HeadFrame{Protocol: "openai.responses.v1", ContentType: "application/json"}}
}
func body() StreamFrame {
	return StreamFrame{Type: FrameBody, Body: &BodyFrame{Data: []byte("\x00opaque\xff")}}
}
func complete() StreamFrame {
	return StreamFrame{Type: FrameComplete, Complete: &CompleteFrame{Outcome: OutcomeSucceeded}}
}

func TestCheckedStreamLifecycle(t *testing.T) {
	p := &scriptedStream{frames: []StreamFrame{head(), body(), complete()}}
	s := NewCheckedStream(p, "openai.responses.v1")
	for _, want := range []FrameType{FrameHead, FrameBody, FrameComplete} {
		f, err := s.Next(context.Background())
		if err != nil || f.Type != want {
			t.Fatalf("frame %s: %+v, %v", want, f, err)
		}
		if want == FrameBody && string(f.Body.Data) != "\x00opaque\xff" {
			t.Fatal("body mutated")
		}
	}
	if _, err := s.Next(context.Background()); err != io.EOF {
		t.Fatalf("after complete: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil || p.closed.Load() != 1 {
		t.Fatalf("close twice: %v, calls %d", err, p.closed.Load())
	}
}

func TestCheckedStreamErrors(t *testing.T) {
	pre := &GatewayError{Code: "rejected", Category: CategoryInvalidRequest, Message: "bad request"}
	p := &scriptedStream{err: pre}
	if _, err := NewCheckedStream(p, "openai.responses.v1").Next(context.Background()); err != pre || p.closed.Load() != 1 {
		t.Fatalf("pre-head error: %v", err)
	}
	if _, err := NewCheckedStream(&scriptedStream{frames: []StreamFrame{{Type: FrameHead, Head: &HeadFrame{Protocol: "openai.responses.v1"}}, complete()}}, "openai.responses.v1").Next(context.Background()); err != nil {
		t.Fatalf("missing rejection content type: %v", err)
	}
	for name, frames := range map[string][]StreamFrame{
		"body before head":        {body()},
		"duplicate head":          {head(), head()},
		"body after complete":     {head(), complete(), body()},
		"duplicate complete":      {head(), complete(), complete()},
		"missing complete":        {head()},
		"empty body":              {head(), {Type: FrameBody, Body: &BodyFrame{}}},
		"head error then success": {{Type: FrameHead, Head: &HeadFrame{Protocol: "openai.responses.v1", ContentType: "c", Error: pre}}, complete()},
		"wrong protocol":          {{Type: FrameHead, Head: &HeadFrame{Protocol: "other", ContentType: "c"}}},
		"failed without cause":    {head(), {Type: FrameComplete, Complete: &CompleteFrame{Outcome: OutcomeFailed}}},
	} {
		t.Run(name, func(t *testing.T) {
			p := &scriptedStream{frames: frames}
			s := NewCheckedStream(p, "openai.responses.v1")
			for range len(frames) + 1 {
				_, err := s.Next(context.Background())
				if err == nil {
					continue
				}
				if !errors.Is(err, ErrStreamContract) || p.closed.Load() != 1 {
					t.Fatalf("want contract error and cleanup, got %v, %d closes", err, p.closed.Load())
				}
				return
			}
			t.Fatal("illegal transition accepted")
		})
	}
}

func TestCheckedStreamCancellation(t *testing.T) {
	p := &scriptedStream{block: true}
	s := NewCheckedStream(p, "openai.responses.v1")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := s.Next(ctx); done <- err }()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) || p.closed.Load() != 1 {
		t.Fatalf("cancel: %v, closes %d", err, p.closed.Load())
	}
	s.Close()
	if p.closed.Load() != 1 {
		t.Fatal("close was not idempotent")
	}
}

func TestCheckedStreamCloseInterruptsNext(t *testing.T) {
	p := &scriptedStream{block: true, entered: make(chan struct{}), release: make(chan struct{})}
	s := NewCheckedStream(p, "openai.responses.v1")
	done := make(chan error, 1)
	go func() { _, err := s.Next(context.Background()); done <- err }()
	<-p.entered
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, context.Canceled) || p.closed.Load() != 1 {
		t.Fatalf("close: %v, calls %d", err, p.closed.Load())
	}
}

func TestCheckedStreamTerminalFailureNotEOF(t *testing.T) {
	upstream := errors.New("upstream disconnected")
	for name, tail := range map[string]error{"close mid-body": nil, "upstream error": upstream, "missing complete": io.EOF} {
		t.Run(name, func(t *testing.T) {
			p := &scriptedStream{frames: []StreamFrame{head(), body()}, err: tail}
			s := NewCheckedStream(p, "openai.responses.v1")
			for range 2 {
				if _, err := s.Next(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			want := tail
			if tail == nil {
				s.Close()
				want = context.Canceled
			}
			if tail == io.EOF {
				want = ErrStreamContract
			}
			for range 2 {
				if _, err := s.Next(context.Background()); !errors.Is(err, want) || err == io.EOF {
					t.Fatalf("terminal failure lost: %v, want %v", err, want)
				}
			}
			if p.closed.Load() != 1 {
				t.Fatalf("close count %d", p.closed.Load())
			}
		})
	}
}

func TestUnknownIsNotZeroOrSafe(t *testing.T) {
	var usage UsageReport
	zero := int64(0)
	if usage.InputTokens != nil || (UsageReport{InputTokens: &zero}).InputTokens == nil {
		t.Fatal("missing tokens became zero")
	}
	var e GatewayError
	if e.RetryDisposition == RetrySafe || e.Retryable {
		t.Fatal("unspecified replay became safe")
	}
}

package claudecode

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
)

const streamStart = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"offline\",\"content\":[],\"usage\":{\"input_tokens\":5}}}\n\n"

func TestStreamingGuaranteesEarlyFrameDelivery(t *testing.T) {
	reader := &eventReader{events: []string{
		streamStart,
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"early\"}}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}}
	s := newMessagesStream(reader)
	defer s.Close()
	if f, err := s.Next(context.Background()); err != nil || f.Type != core.FrameHead || reader.reads != 1 {
		t.Fatalf("Head=%+v reads=%d err=%v", f, reader.reads, err)
	}
	if f, err := s.Next(context.Background()); err != nil || f.Type != core.FrameBody || !strings.Contains(string(f.Body.Data), "response.created") || reader.reads != 1 {
		t.Fatalf("initial body=%+v reads=%d err=%v", f, reader.reads, err)
	}
	for {
		f, err := s.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if f.Type == core.FrameBody && strings.Contains(string(f.Body.Data), `"delta":"early"`) {
			if reader.reads != 3 {
				t.Fatalf("first delta required %d reads, want 3 (not EOF)", reader.reads)
			}
			return
		}
	}
}

func TestStreamingGuaranteesBackpressure(t *testing.T) {
	reader := &eventReader{events: []string{streamStart,
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"one\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"two\"}}\n\n",
	}}
	s := newMessagesStream(reader)
	defer s.Close()
	if _, err := s.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	if reader.reads != 1 {
		t.Fatalf("read ahead before consumer advanced: %d", reader.reads)
	}
	if _, err := s.Next(context.Background()); err != nil { // block start; queues a second frame
		t.Fatal(err)
	}
	if _, err := s.Next(context.Background()); err != nil { // pending part-added must not read
		t.Fatal(err)
	}
	if reader.reads != 2 {
		t.Fatalf("pending frame triggered read-ahead: %d", reader.reads)
	}
	if _, err := s.Next(context.Background()); err != nil { // first delta
		t.Fatal(err)
	}
	if reader.reads != 3 {
		t.Fatalf("first delta read count=%d, want 3", reader.reads)
	}
	if _, err := s.Next(context.Background()); err != nil || reader.reads != 4 {
		t.Fatalf("next delta read count=%d err=%v", reader.reads, err)
	}
}

func TestStreamingGuaranteesCancellation(t *testing.T) {
	preCanceled, cancelPre := context.WithCancel(context.Background())
	cancelPre()
	preBody := &eventReader{events: []string{streamStart}}
	pre := newMessagesStream(preBody)
	if _, err := pre.Next(preCanceled); !errors.Is(err, context.Canceled) || preBody.reads != 0 {
		t.Fatalf("pre-canceled Next err=%v reads=%d", err, preBody.reads)
	}
	if got := pre.usage(core.UsagePartial); got.Source != core.UsageUnknown || got.InputTokens != nil || got.OutputTokens != nil {
		t.Fatalf("pre-canceled usage=%+v", got)
	}
	if _, err := pre.Next(context.Background()); err != io.EOF {
		t.Fatalf("post pre-cancel Next err=%v", err)
	}

	for _, tc := range []struct {
		name string
		ctx  func() (context.Context, context.CancelFunc, error)
	}{
		{"cancel", func() (context.Context, context.CancelFunc, error) {
			ctx, cancel := context.WithCancel(context.Background())
			return ctx, cancel, context.Canceled
		}},
		{"deadline", func() (context.Context, context.CancelFunc, error) {
			ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(500*time.Millisecond))
			return ctx, func() { cancel() }, context.DeadlineExceeded
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel, wantErr := tc.ctx()
			defer cancel()
			body := newBlockingReadCloser(streamStart)
			s := newMessagesStream(body)
			if f, err := s.Next(ctx); err != nil || f.Type != core.FrameHead {
				t.Fatalf("Head=%+v err=%v", f, err)
			}
			if f, err := s.Next(ctx); err != nil || f.Type != core.FrameBody {
				t.Fatalf("initial Body=%+v err=%v", f, err)
			}
			result := make(chan error, 1)
			go func() { _, err := s.Next(ctx); result <- err }()
			<-body.entered
			if tc.name == "deadline" {
				// Let the real deadline expire while the body is blocked in Read.
			} else {
				cancel()
			}
			select {
			case err := <-result:
				if !errors.Is(err, wantErr) {
					t.Fatalf("blocked Next error=%v, want %v", err, wantErr)
				}
			case <-time.After(time.Second):
				t.Fatal("context cancellation did not close the blocking body")
			}
			if body.closes.Load() != 1 {
				t.Fatalf("body close count=%d", body.closes.Load())
			}
			if got := s.usage(core.UsagePartial); got.Source != core.UsageProvider || !sameCount(got.InputTokens, int64Ptr(5)) || got.OutputTokens != nil || got.Completeness != core.UsagePartial {
				t.Fatalf("canceled stream usage=%+v", got)
			}
			if f, err := s.Next(context.Background()); err != io.EOF || f.Type != "" {
				t.Fatalf("post-cancel result=%+v err=%v", f, err)
			}
			if f, err := s.Next(context.Background()); err != io.EOF || f.Type != "" {
				t.Fatalf("repeated post-cancel result=%+v err=%v", f, err)
			}
		})
	}
}

func TestStreamingGuaranteesTerminalAccounting(t *testing.T) {
	complete := "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n" + "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	noUsageStart := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"offline\",\"content\":[],\"usage\":{}}}\n\n"
	for _, tc := range []struct {
		name     string
		reader   io.ReadCloser
		outcome  core.Outcome
		code     string
		category core.ErrorCategory
		input    *int64
		output   *int64
		source   core.UsageSource
		complete core.UsageCompleteness
	}{
		{
			name: "success", reader: io.NopCloser(strings.NewReader(streamStart + complete)),
			outcome: core.OutcomeSucceeded, input: int64Ptr(5), output: int64Ptr(3),
			source: core.UsageProvider, complete: core.UsageComplete,
		},
		{
			name: "IO failure with partial usage", reader: newFailingReadCloser(streamStart, errors.New("synthetic upstream read failure")),
			outcome: core.OutcomeFailed, code: "upstream_read_error", category: core.CategoryUnavailable,
			input: int64Ptr(5), source: core.UsageProvider, complete: core.UsagePartial,
		},
		{
			name: "IO cancellation", reader: newFailingReadCloser(streamStart, context.Canceled),
			outcome: core.OutcomeCancelled, code: "upstream_cancelled", category: core.CategoryCancelled,
			input: int64Ptr(5), source: core.UsageProvider, complete: core.UsagePartial,
		},
		{
			name: "IO deadline", reader: newFailingReadCloser(streamStart, context.DeadlineExceeded),
			outcome: core.OutcomeIncomplete, code: "upstream_timeout", category: core.CategoryTimeout,
			input: int64Ptr(5), source: core.UsageProvider, complete: core.UsagePartial,
		},
		{
			name: "IO failure without usage", reader: newFailingReadCloser(noUsageStart, errors.New("synthetic upstream read failure")),
			outcome: core.OutcomeFailed, code: "upstream_read_error", category: core.CategoryUnavailable,
			source: core.UsageUnknown, complete: core.UsagePartial,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newMessagesStream(tc.reader)
			defer s.Close()
			completes := 0
			for range 20 {
				f, err := s.Next(context.Background())
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if f.Type != core.FrameComplete {
					continue
				}
				completes++
				got := f.Complete
				if got.Outcome != tc.outcome || got.Usage == nil || !sameCount(got.Usage.InputTokens, tc.input) || !sameCount(got.Usage.OutputTokens, tc.output) || got.Usage.Source != tc.source || got.Usage.Completeness != tc.complete {
					t.Fatalf("Complete=%+v", got)
				}
				if tc.code == "" {
					if got.Error != nil {
						t.Fatalf("successful Complete has error: %+v", got.Error)
					}
				} else if got.Error == nil || got.Error.Code != tc.code || got.Error.Category != tc.category {
					t.Fatalf("classified failure=%+v, want %s/%s", got.Error, tc.code, tc.category)
				}
			}
			if completes != 1 {
				t.Fatalf("Complete count=%d", completes)
			}
			if r, ok := tc.reader.(*failingReadCloser); ok && r.closes.Load() != 1 {
				t.Fatalf("failed upstream body close count=%d", r.closes.Load())
			}
			if f, err := s.Next(context.Background()); err != io.EOF || f.Type != "" {
				t.Fatalf("post-terminal result=%+v err=%v", f, err)
			}
		})
	}
}

func TestStreamingGuaranteesConsumerCloseInterruptsIO(t *testing.T) {
	body := newBlockingReadCloser(streamStart)
	s := newMessagesStream(body)
	if f, err := s.Next(context.Background()); err != nil || f.Type != core.FrameHead {
		t.Fatalf("Head=%+v err=%v", f, err)
	}
	if f, err := s.Next(context.Background()); err != nil || f.Type != core.FrameBody {
		t.Fatalf("initial Body=%+v err=%v", f, err)
	}
	result := make(chan error, 1)
	go func() { _, err := s.Next(context.Background()); result <- err }()
	<-body.entered
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("blocked Next after consumer close=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not interrupt blocked IO")
	}
	if body.closes.Load() != 1 {
		t.Fatalf("body close count=%d", body.closes.Load())
	}
	if got := s.usage(core.UsagePartial); got.Source != core.UsageProvider || !sameCount(got.InputTokens, int64Ptr(5)) || got.OutputTokens != nil || got.Completeness != core.UsagePartial {
		t.Fatalf("consumer-close usage=%+v", got)
	}
	if f, err := s.Next(context.Background()); err != io.EOF || f.Type != "" {
		t.Fatalf("post-close result=%+v err=%v", f, err)
	}
}

func int64Ptr(v int64) *int64 { return &v }

func sameCount(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

type eventReader struct {
	events []string
	reads  int
}

func (r *eventReader) Read(p []byte) (int, error) {
	if r.reads == len(r.events) {
		return 0, io.EOF
	}
	event := r.events[r.reads]
	r.reads++
	return copy(p, event), nil
}
func (*eventReader) Close() error { return nil }

type blockingReadCloser struct {
	first   []byte
	entered chan struct{}
	release chan struct{}
	reads   atomic.Int32
	closes  atomic.Int32
	once    sync.Once
}

func newBlockingReadCloser(first string) *blockingReadCloser {
	return &blockingReadCloser{first: []byte(first), entered: make(chan struct{}), release: make(chan struct{})}
}

func (r *blockingReadCloser) Read(p []byte) (int, error) {
	if r.reads.Add(1) == 1 {
		return copy(p, r.first), nil
	}
	r.once.Do(func() { close(r.entered) })
	<-r.release
	return 0, io.ErrClosedPipe
}

func (r *blockingReadCloser) Close() error {
	if r.closes.Add(1) == 1 {
		close(r.release)
	}
	return nil
}

type failingReadCloser struct {
	data   []byte
	sent   bool
	err    error
	closes atomic.Int32
}

func newFailingReadCloser(data string, err error) *failingReadCloser {
	return &failingReadCloser{data: []byte(data), err: err}
}
func (r *failingReadCloser) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, r.data), nil
	}
	return 0, r.err
}
func (r *failingReadCloser) Close() error { r.closes.Add(1); return nil }

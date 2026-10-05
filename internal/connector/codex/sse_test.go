package codex

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
)

func feedSSE(chunks ...[]byte) sseObserver {
	var observer sseObserver
	for _, chunk := range chunks {
		observer.feed(chunk)
	}
	return observer
}

func TestSSEObserverSplitBoundariesAndOpaqueForwarding(t *testing.T) {
	input := []byte(": keepalive\r\nevent: response.output_text.delta\r\ndata: {\r\ndata: \"delta\":\"caf\xc3\xa9\"}\r\n\r\n")
	want := feedSSE(input)
	if err := want.finish(); err != nil || want.events != 1 || want.lastEvent != "response.output_text.delta" {
		t.Fatalf("baseline observer = %+v, finish %v", want, err)
	}
	lf := feedSSE(bytes.ReplaceAll(input, []byte("\r\n"), []byte("\n")))
	if err := lf.finish(); err != nil || lf.events != want.events || lf.lastEvent != want.lastEvent {
		t.Fatalf("LF observer = %+v, finish %v", lf, err)
	}
	for split := range len(input) + 1 {
		got := feedSSE(input[:split], input[split:])
		if err := got.finish(); err != nil || got.events != want.events || got.lastEvent != want.lastEvent {
			t.Fatalf("split %d observer = %+v, finish %v", split, got, err)
		}
		body := &fragmentedSSEBody{data: input, split: split}
		stream := newTestExecuteStream(body, http.StatusOK)
		out, _ := drainExecuteStream(t, stream)
		if !bytes.Equal(out, input) {
			t.Fatalf("split %d changed forwarded bytes", split)
		}
	}
	chunks := make([][]byte, len(input))
	for i := range input {
		chunks[i] = input[i : i+1]
	}
	got := feedSSE(chunks...)
	if err := got.finish(); err != nil || got.events != want.events || got.lastEvent != want.lastEvent {
		t.Fatalf("one-byte observer = %+v, finish %v", got, err)
	}

	body := &countedBody{reader: bytes.NewReader(input)}
	stream := newTestExecuteStream(body, http.StatusOK)
	out, _ := drainExecuteStream(t, stream)
	if !bytes.Equal(out, input) {
		t.Fatalf("forwarded bytes changed: got %q want %q", out, input)
	}
}

func TestSSEObserverBoundsAndMalformedFraming(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input []byte
	}{
		{"event limit", []byte("data:" + strings.Repeat("x", maxSSEEventBytes) + "\n\n")},
		{"scalar limit", []byte("event:" + strings.Repeat("x", maxSSEScalarBytes+1) + "\n\n")},
		{"invalid utf8", []byte{'d', 'a', 't', 'a', ':', ' ', 0xff, '\n', '\n'}},
		{"incomplete line", []byte("data: partial")},
		{"incomplete event", []byte("data: complete line\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observer := feedSSE(tc.input)
			if observer.err == nil && observer.finish() == nil {
				t.Fatal("malformed or oversized framing accepted")
			}
		})
	}
	valid := feedSSE([]byte("data: x\n\n"))
	if err := valid.finish(); err != nil || valid.events != 1 {
		t.Fatalf("valid event rejected: %+v, %v", valid, err)
	}
}

func TestSSEObserverCloseDuringIncompleteEvent(t *testing.T) {
	body := &incompleteThenBlockBody{started: make(chan struct{}), release: make(chan struct{})}
	stream := newTestExecuteStream(body, http.StatusOK)
	if _, err := stream.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	frame, err := stream.Next(context.Background())
	if err != nil || frame.Type != core.FrameBody {
		t.Fatalf("body frame = %+v, %v", frame, err)
	}
	next := make(chan struct{})
	go func() { _, _ = stream.Next(context.Background()); close(next) }()
	select {
	case <-body.started:
	case <-time.After(time.Second):
		t.Fatal("stream did not block after the incomplete event prefix")
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close err=%v", err)
	}
	select {
	case <-next:
	case <-time.After(time.Second):
		t.Fatal("Close did not interrupt upstream read")
	}
	if body.closes.Load() != 1 {
		t.Fatalf("body close count=%d", body.closes.Load())
	}
}

type incompleteThenBlockBody struct {
	started chan struct{}
	release chan struct{}
	first   bool
	closes  atomic.Int32
}

type fragmentedSSEBody struct {
	data  []byte
	split int
	pos   int
}

func (b *fragmentedSSEBody) Read(p []byte) (int, error) {
	if b.pos == len(b.data) {
		return 0, io.EOF
	}
	end := len(b.data)
	if b.pos < b.split && b.split < end {
		end = b.split
	}
	n := copy(p, b.data[b.pos:end])
	b.pos += n
	return n, nil
}

func (*fragmentedSSEBody) Close() error { return nil }

func (b *incompleteThenBlockBody) Read(p []byte) (int, error) {
	if !b.first {
		b.first = true
		return copy(p, []byte("data: incomplete")), nil
	}
	close(b.started)
	<-b.release
	return 0, context.Canceled
}

func (b *incompleteThenBlockBody) Close() error {
	if b.closes.Add(1) == 1 {
		close(b.release)
	}
	return nil
}

package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

type chunkReader struct {
	data []byte
	size int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := min(len(p), min(r.size, len(r.data)))
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

func collectMessagesEvents(t *testing.T, raw []byte, chunk int) []messagesSSEEvent {
	t.Helper()
	p := newMessagesSSEReader(io.NopCloser(&chunkReader{data: raw, size: chunk}))
	var events []messagesSSEEvent
	for {
		e, err := p.next(context.Background())
		if err == io.EOF {
			return events
		}
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, e)
	}
}

func TestMessagesSSEFixturesFragmented(t *testing.T) {
	for _, name := range []string{"stream-normal.sse", "stream-max-tokens.sse", "stream-error.sse", "stream-premature-eof.sse"} {
		t.Run(name, func(t *testing.T) {
			raw := fixture(t, name)
			wantRaw := raw
			if name == "stream-premature-eof.sse" {
				if bytes.HasSuffix(raw, []byte("\n\n")) {
					t.Fatal("premature fixture unexpectedly has a blank-line event terminator")
				}
				lastComplete := bytes.LastIndex(raw, []byte("\n\n"))
				if lastComplete < 0 {
					t.Fatal("premature fixture has no complete preceding events")
				}
				wantRaw = raw[:lastComplete+2]
			}
			want := parseFixtureEvents(t, wantRaw)
			for chunk := 1; chunk <= len(raw)+1; chunk++ {
				got := collectMessagesEvents(t, raw, chunk)
				if len(got) != len(want) {
					t.Fatalf("chunk %d: got %d events, want %d", chunk, len(got), len(want))
				}
				for i := range got {
					if got[i].typeName != want[i].name || string(got[i].data) != want[i].data {
						t.Fatalf("chunk %d event %d differs", chunk, i)
					}
				}
			}
		})
	}
}

func TestMessagesSSEFramingAndBounds(t *testing.T) {
	input := ": ping\r\nevent: ping\r\ndata: {\"type\":\r\ndata: \"ping\",\"text\":\"雪🪲\"}\r\n\r\n"
	events := collectMessagesEvents(t, []byte(input), 1)
	if len(events) != 1 || events[0].typeName != "ping" || !json.Valid(events[0].data) || !strings.Contains(string(events[0].data), "雪🪲") {
		t.Fatalf("multiline UTF-8 event: %+v", events)
	}
	line := `data: {"type":"ping","x":"` + strings.Repeat("x", maxSSEBytes-len(`data: {"type":"ping","x":"`)-2) + `"}`
	if len(line) != maxSSEBytes {
		t.Fatalf("boundary fixture line=%d", len(line))
	}
	events = collectMessagesEvents(t, []byte(line+"\n\n"), 1024)
	if len(events) != 1 {
		t.Fatalf("1 MiB line rejected: %d events", len(events))
	}
	for _, raw := range [][]byte{[]byte(line + "x\n\n"), []byte("data: {\"type\":\"ping\"}\n" + strings.Repeat("data:  \n", maxSSEBytes/2))} {
		p := newMessagesSSEReader(io.NopCloser(bytes.NewReader(raw)))
		for {
			_, err := p.next(context.Background())
			if err != nil {
				if !errors.Is(err, errSSETooLarge) {
					t.Fatalf("oversize error = %v", err)
				}
				break
			}
		}
	}
}

type cancelReader struct{ closed chan struct{} }

func (r *cancelReader) Read([]byte) (int, error) { <-r.closed; return 0, io.ErrClosedPipe }
func (r *cancelReader) Close() error {
	select {
	case <-r.closed:
	default:
		close(r.closed)
	}
	return nil
}

func TestMessagesSSECancellation(t *testing.T) {
	r := &cancelReader{closed: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := newMessagesSSEReader(r).next(ctx); done <- err }()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("next error = %v", err)
	}
}

func TestMessagesSSERejectsUnknownSemanticAndMalformed(t *testing.T) {
	for _, raw := range []string{"event: message_start\ndata: {\"type\":\"message_stop\"}\n\n", "event: future\ndata: {\"type\":\"future\"}\n\n", "data: {\"type\":\n\n"} {
		_, err := newMessagesSSEReader(io.NopCloser(strings.NewReader(raw))).next(context.Background())
		if err == nil {
			t.Fatalf("accepted malformed/unsupported stream %q", raw)
		}
	}
}

func TestMessagesSSEEOFDoesNotDispatchUnterminatedEvents(t *testing.T) {
	for _, raw := range []string{
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n",
		"data: {\"type\":\"ping\"}\n",
		"\n",
	} {
		p := newMessagesSSEReader(io.NopCloser(strings.NewReader(raw)))
		if event, err := p.next(context.Background()); err != io.EOF || event.typeName != "" {
			t.Fatalf("EOF dispatched event %+v, err=%v for %q", event, err, raw)
		}
	}
}

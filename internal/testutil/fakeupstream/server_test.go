package fakeupstream

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for upstream")
		var zero T
		return zero
	}
}

func TestCaptureAndFixedResponse(t *testing.T) {
	s := New(Response{Status: http.StatusTeapot, Header: http.Header{"Content-Type": {"application/json"}, "X-Test": {"a", "b"}}, Body: []byte(`{ "ok":false }`)})
	defer s.Close()
	want := []byte("{ \"unrecognized\": [1, {\"x\":true}] }\n")
	req, err := http.NewRequest(http.MethodPost, s.URL+"/v1/responses?q=one%20two", bytes.NewReader(want))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Custom", "preserved")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	got := receive(t, s.Requests)
	if got.Method != http.MethodPost || got.Path != "/v1/responses" || got.RawQuery != "q=one%20two" || got.Header.Get("X-Custom") != "preserved" || !bytes.Equal(got.Body, want) {
		t.Fatalf("capture differs: %+v", got)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil || res.StatusCode != http.StatusTeapot || res.Header.Get("Content-Type") != "application/json" || len(res.Header.Values("X-Test")) != 2 || !bytes.Equal(body, []byte(`{ "ok":false }`)) {
		t.Fatalf("response: status=%d headers=%v body=%q err=%v", res.StatusCode, res.Header, body, err)
	}
}

func TestGatedSSE(t *testing.T) {
	first := make(chan struct{})
	second := make(chan struct{})
	sent := make(chan struct{})
	s := New(Response{Steps: []Step{
		{Gate: first, Sent: sent, Data: []byte("data: one\n\n")},
		{Gate: second, Data: []byte("data: two\n\n")},
	}})
	defer s.Close()
	defer func() {
		select {
		case <-first:
		default:
			close(first)
		}
	}()
	defer func() {
		select {
		case <-second:
		default:
			close(second)
		}
	}() // Unblock the server even if an assertion fails.
	res, err := http.Get(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	receive(t, s.Requests)
	if res.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("content type: %q", res.Header.Get("Content-Type"))
	}
	close(first)
	receive(t, sent)
	chunk := make([]byte, len("data: one\n\n"))
	if _, err := io.ReadFull(res.Body, chunk); err != nil || string(chunk) != "data: one\n\n" {
		t.Fatalf("first chunk = %q, %v", chunk, err)
	}
	// The second gate is still closed: the first event arrived while the
	// upstream handler cannot finish, independent of scheduling or sleeps.
	close(second)
	rest, err := io.ReadAll(res.Body)
	if err != nil || string(rest) != "data: two\n\n" {
		t.Fatalf("remaining stream = %q, %v", rest, err)
	}
}

func TestDrops(t *testing.T) {
	for _, tc := range []struct {
		name string
		resp Response
	}{
		{"before", Response{Drop: true}},
		{"midstream", Response{Steps: []Step{{Data: []byte("data: one\n\n"), Drop: true}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(tc.resp)
			defer s.Close()
			res, err := http.Get(s.URL)
			receive(t, s.Requests)
			if tc.resp.Drop {
				if err == nil {
					res.Body.Close()
					t.Fatal("expected connection failure before headers")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			body, err := io.ReadAll(res.Body)
			if !strings.HasPrefix(string(body), "data: one\n\n") || err != io.ErrUnexpectedEOF {
				t.Fatalf("midstream body=%q err=%v", body, err)
			}
		})
	}
}

func TestCancellation(t *testing.T) {
	gate := make(chan struct{})
	s := New(Response{Steps: []Step{{Gate: gate, Data: []byte("data: never\n\n")}}})
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	got := receive(t, s.Requests)
	cancel()
	receive(t, got.Cancelled)
}

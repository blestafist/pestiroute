package codex

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
)

func executeFixture(t *testing.T, handler http.HandlerFunc) (*Connector, core.InvocationServices, *atomic.Int32) {
	t.Helper()
	var sends atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sends.Add(1)
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	c := NewConnector()
	c.endpoint = server.URL
	c.streamIdleTimeout = time.Second
	if err := c.Init(t.Context(), core.ComponentConfig{Data: []byte(`{"model":"gpt-5.4-mini","account_id":"account-a","profile":"codex-responses-http-sse-v1"}`)}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	credential := mustOAuthBundle(t, "access-token", "account-a", time.Now().Add(time.Hour))
	return c, core.InvocationServices{Credentials: headerCredentials(credential)}, &sends
}

func executeRequest(body []byte) core.ExecutionRequest {
	return responsesValidationRequest(body)
}

func TestExecuteStreamingSingleSendAndOpaqueFrames(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 12<<10)
	request := executeRequest(append([]byte(`{"model":"gpt-5.4-mini","input":"`), append(payload, []byte(`","stream":true,"store":false}`)...)...))
	var received []byte
	c, services, sends := executeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/" || r.GetBody != nil || r.ContentLength != int64(len(request.Payload.Body)) || r.Header.Get("Accept-Encoding") != "identity" ||
			r.Header.Get("Authorization") != "Bearer access-token" || r.Header.Get("ChatGPT-Account-Id") != "account-a" || r.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("unexpected upstream request: method=%s path=%s length=%d headers=%v", r.Method, r.URL.Path, r.ContentLength, r.Header)
		}
		var err error
		received, err = io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	})
	got, ge := c.Execute(t.Context(), request, core.AttemptScope{Mode: core.ModeNative, AccountID: "account-a"}, services)
	if ge != nil {
		t.Fatal(ge)
	}
	defer got.Stream.Close()
	frame, err := got.Stream.Next(t.Context())
	if err != nil || frame.Type != core.FrameHead || frame.Head.HTTPStatus == nil || *frame.Head.HTTPStatus != http.StatusOK {
		t.Fatalf("head=%+v err=%v", frame, err)
	}
	var output []byte
	for {
		frame, err = got.Stream.Next(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if frame.Type == core.FrameComplete {
			if frame.Complete.Outcome != core.OutcomeIncomplete || frame.Complete.Error == nil {
				t.Fatalf("unverified EOF completion: %+v", frame.Complete)
			}
			break
		}
		if frame.Type != core.FrameBody || len(frame.Body.Data) > executeChunkSize {
			t.Fatalf("unexpected frame: %+v", frame)
		}
		output = append(output, frame.Body.Data...)
	}
	if !bytes.Equal(output, payload) || !bytes.Equal(received, request.Payload.Body) || sends.Load() != 1 {
		t.Fatalf("output=%d request=%d sends=%d", len(output), len(received), sends.Load())
	}
}

func TestExecuteCloseAndCancellationReleaseReader(t *testing.T) {
	for _, cancelContext := range []bool{false, true} {
		t.Run(map[bool]string{false: "close", true: "cancel"}[cancelContext], func(t *testing.T) {
			started, canceled := make(chan struct{}), make(chan struct{})
			c, services, sends := executeFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("first"))
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				close(started)
				<-r.Context().Done()
				close(canceled)
			})
			ctx, cancel := context.WithCancel(t.Context())
			response, ge := c.Execute(ctx, executeRequest(loadResponsesFixture(t, "request-positive.json")), core.AttemptScope{Mode: core.ModeNative, AccountID: "account-a"}, services)
			if ge != nil {
				t.Fatal(ge)
			}
			if frame, err := response.Stream.Next(ctx); err != nil || frame.Type != core.FrameHead {
				t.Fatalf("Head=%+v err=%v", frame, err)
			}
			if frame, err := response.Stream.Next(ctx); err != nil || frame.Type != core.FrameBody {
				t.Fatalf("Body=%+v err=%v", frame, err)
			}
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("handler did not start streaming")
			}
			if cancelContext {
				cancel()
			} else if err := response.Stream.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-canceled:
			case <-time.After(time.Second):
				t.Fatal("stream close/cancellation did not abort upstream")
			}
			if sends.Load() != 1 {
				t.Fatalf("sends=%d", sends.Load())
			}
			cancel()
		})
	}
}

func TestExecuteTranslationAdaptsBodyOnce(t *testing.T) {
	c, services, sends := executeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte(`"stream":true`)) || !bytes.Contains(body, []byte(`"store":false`)) || !bytes.Contains(body, []byte(`"reasoning.encrypted_content"`)) {
			t.Errorf("translation defaults missing: %s", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	request := executeRequest([]byte(`{"model":"gpt-5.4-mini","input":"hello"}`))
	response, ge := c.Execute(t.Context(), request, core.AttemptScope{Mode: core.ModeTranslation, AccountID: "account-a"}, services)
	if ge != nil {
		t.Fatal(ge)
	}
	defer response.Stream.Close()
	for range 3 {
		if _, err := response.Stream.Next(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if sends.Load() != 1 {
		t.Fatalf("sends=%d", sends.Load())
	}
}

func TestExecuteHTTPRejectionIsFailedAndClosesBody(t *testing.T) {
	closed := make(chan struct{})
	c, services, sends := executeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("rejected"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		go func() {
			<-r.Context().Done()
			close(closed)
		}()
	})
	response, ge := c.Execute(t.Context(), executeRequest(loadResponsesFixture(t, "request-positive.json")), core.AttemptScope{Mode: core.ModeNative, AccountID: "account-a"}, services)
	if ge != nil {
		t.Fatal(ge)
	}
	defer response.Stream.Close()
	head, err := response.Stream.Next(t.Context())
	if err != nil || head.Head.Error == nil || head.Head.Error.Category != core.CategoryUnauthenticated {
		t.Fatalf("rejection Head=%+v err=%v", head, err)
	}
	frame, err := response.Stream.Next(t.Context())
	if err != nil || frame.Type != core.FrameBody || string(frame.Body.Data) != "rejected" {
		t.Fatalf("rejection Body=%+v err=%v", frame, err)
	}
	frame, err = response.Stream.Next(t.Context())
	if err != nil || frame.Type != core.FrameComplete || frame.Complete.Outcome != core.OutcomeFailed {
		t.Fatalf("rejection completion=%+v err=%v", frame, err)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("HTTP rejection body was not released")
	}
	if sends.Load() != 1 {
		t.Fatalf("sends=%d", sends.Load())
	}
}

func TestExecuteHTTPRejectionLongJSONBodyBypassesSSEObserver(t *testing.T) {
	body := []byte(`{"error":"` + strings.Repeat("x", 5000) + `"}`)
	c, services, _ := executeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(body)
	})
	response, ge := c.Execute(t.Context(), executeRequest(loadResponsesFixture(t, "request-positive.json")), core.AttemptScope{Mode: core.ModeNative, AccountID: "account-a"}, services)
	if ge != nil {
		t.Fatal(ge)
	}
	defer response.Stream.Close()
	head, err := response.Stream.Next(t.Context())
	if err != nil || head.Type != core.FrameHead || head.Head.Error == nil || head.Head.Error.Category != core.CategoryInvalidRequest {
		t.Fatalf("rejection Head=%+v err=%v", head, err)
	}
	var output []byte
	for {
		frame, err := response.Stream.Next(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if frame.Type == core.FrameComplete {
			if frame.Complete.Outcome != core.OutcomeFailed || frame.Complete.Error == nil || frame.Complete.Error.Code != "upstream_rejection" {
				t.Fatalf("rejection completion=%+v", frame.Complete)
			}
			break
		}
		if frame.Type != core.FrameBody {
			t.Fatalf("unexpected rejection frame: %+v", frame)
		}
		output = append(output, frame.Body.Data...)
	}
	if !bytes.Equal(output, body) {
		t.Fatalf("rejection body changed: got %d bytes, want %d", len(output), len(body))
	}
}

func TestExecuteTransportFailureIsSanitized(t *testing.T) {
	c, services, sends := executeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close()
	})
	response, ge := c.Execute(t.Context(), executeRequest(loadResponsesFixture(t, "request-positive.json")), core.AttemptScope{Mode: core.ModeNative, AccountID: "account-a"}, services)
	if ge == nil || ge.Code != "upstream_unavailable" || ge.OriginalError != "" || ge.Message != "Upstream request failed" || response.Stream != nil {
		t.Fatalf("response=%+v error=%+v", response, ge)
	}
	if sends.Load() != 1 {
		t.Fatalf("sends=%d", sends.Load())
	}
}

func TestExecuteManagedClientRefusesRedirect(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Add(1) }))
	defer target.Close()
	c, services, sends := executeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusSeeOther)
	})
	services.Transport = &http.Client{Transport: http.DefaultTransport}
	response, ge := c.Execute(t.Context(), executeRequest(loadResponsesFixture(t, "request-positive.json")), core.AttemptScope{Mode: core.ModeNative, AccountID: "account-a"}, services)
	if ge != nil {
		t.Fatal(ge)
	}
	defer response.Stream.Close()
	head, err := response.Stream.Next(t.Context())
	if err != nil || head.Head.HTTPStatus == nil || *head.Head.HTTPStatus != http.StatusSeeOther || head.Head.Error == nil || http.Header(head.Head.Headers).Get("Location") != "" {
		t.Fatalf("redirect Head=%+v err=%v", head, err)
	}
	if redirected.Load() != 0 || sends.Load() != 1 {
		t.Fatalf("redirect target sends=%d original sends=%d", redirected.Load(), sends.Load())
	}
}

func TestExecuteRejectsEncodedResponse(t *testing.T) {
	c, services, sends := executeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not decoded"))
	})
	response, ge := c.Execute(t.Context(), executeRequest(loadResponsesFixture(t, "request-positive.json")), core.AttemptScope{Mode: core.ModeNative, AccountID: "account-a"}, services)
	if ge == nil || ge.Code != "unsupported_response_encoding" || response.Stream != nil || sends.Load() != 1 {
		t.Fatalf("response=%+v error=%+v sends=%d", response, ge, sends.Load())
	}
}

func TestExecuteDoesNotUseManagedClientCookieJar(t *testing.T) {
	var cookie string
	c, services, _ := executeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		cookie = r.Header.Get("Cookie")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("opaque"))
	})
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := url.Parse(c.endpoint)
	if err != nil {
		t.Fatal(err)
	}
	jar.SetCookies(endpoint, []*http.Cookie{{Name: "session", Value: "must-not-forward"}})
	services.Transport = &http.Client{Transport: http.DefaultTransport, Jar: jar}
	response, ge := c.Execute(t.Context(), executeRequest(loadResponsesFixture(t, "request-positive.json")), core.AttemptScope{Mode: core.ModeNative, AccountID: "account-a"}, services)
	if ge != nil {
		t.Fatal(ge)
	}
	defer response.Stream.Close()
	if _, err := response.Stream.Next(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := response.Stream.Next(t.Context()); err != nil {
		t.Fatal(err)
	}
	if cookie != "" {
		t.Fatalf("runtime client CookieJar leaked cookie: %q", cookie)
	}
	if managed := services.Transport.(*http.Client); managed.Jar != jar || managed.Transport != http.DefaultTransport {
		t.Fatal("connector mutated the runtime-owned HTTP client")
	}
}

func TestExecuteInjectedIdleTimeout(t *testing.T) {
	started, canceled := make(chan struct{}), make(chan struct{})
	c, services, _ := executeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("first"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		close(started)
		<-r.Context().Done()
		close(canceled)
	})
	c.streamIdleTimeout = 25 * time.Millisecond
	response, ge := c.Execute(t.Context(), executeRequest(loadResponsesFixture(t, "request-positive.json")), core.AttemptScope{Mode: core.ModeNative, AccountID: "account-a"}, services)
	if ge != nil {
		t.Fatal(ge)
	}
	defer response.Stream.Close()
	for range 2 {
		if _, err := response.Stream.Next(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("stream did not start")
	}
	frame, err := response.Stream.Next(t.Context())
	if err != nil || frame.Type != core.FrameComplete || frame.Complete.Outcome != core.OutcomeIncomplete || frame.Complete.Error == nil || frame.Complete.Error.Category != core.CategoryTimeout {
		t.Fatalf("idle completion=%+v err=%v", frame, err)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("idle timeout did not cancel upstream")
	}
}

//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	adapter "github.com/blestafist/pestiroute/internal/adapter/responses"
	"github.com/blestafist/pestiroute/internal/connector/codex"
	"github.com/blestafist/pestiroute/internal/core"
	secure "github.com/blestafist/pestiroute/internal/crypto"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

func TestM6ProtectedExternalFallback(t *testing.T) {
	var sent atomic.Int32
	capturedBodies := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ := io.ReadAll(r.Body)
		capturedBodies <- capturedBody
		if r.Header.Get("Authorization") != "Bearer synthetic" {
			t.Errorf("external fallback credential=%q", r.Header.Get("Authorization"))
		}
		sent.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"fallback-response","status":"completed","usage":{"input_tokens":1,"output_tokens":2}}`)
	}))
	defer upstream.Close()
	_, dbPath, keyPath := protectedFixture(t)
	seedExternalFallbackAccount(t, dbPath, keyPath)
	child := buildExternalResponsesChild(t)
	c := fixtureProtectedConfig(dbPath, keyPath, "127.0.0.1:0")
	first := protectedConnector{ID: "upstream", Kind: "connector", Implementation: "pestiroute.codex.responses", Protocols: []string{responsesProtocol}, Settings: nativeSettings{Profile: "codex-responses-http-sse-v1", Model: "model-a", AccountID: "account-a"}}
	second := protectedConnector{ID: "upstream-b", Kind: "connector", Implementation: "pestiroute.responses.native", Protocols: []string{responsesProtocol}, Settings: nativeSettings{
		BaseURL: upstream.URL + "/v1/responses", UpstreamProtocol: responsesProtocol, Mode: "native", CredentialEnv: "PROTECTED_TEST_CREDENTIAL_B",
		MaxRequestBodyBytes: 1 << 20, MaxRequestHeaderBytes: 8192, ConnectTimeout: "1s", TLSHandshakeTimeout: "1s", ResponseHeaderTimeout: "1s", StreamIdleTimeout: "1s",
	}, Process: &protectedProcess{Executable: child, WorkingDirectory: t.TempDir()}}
	c.protected.Connectors = []protectedConnector{first, second}
	c.protected.Routes[0].Targets = []routeTarget{{Connector: "upstream", Account: "account-a"}, {Connector: "upstream-b", Account: "account-b"}}
	maxAttempts := 2
	c.protected.Routes[0].Retry = &retryConfig{MaxAttempts: &maxAttempts, Deadline: "5s"}
	prepared, err := prepareProtectedConfig(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := sqlite.NewKeyPolicies(prepared.runtimeDB).GetLatest(context.Background(), "policy-id-a")
	if err != nil {
		t.Fatal(err)
	}
	issued, err := sqlite.NewVirtualKeys(prepared.runtimeDB).Create(context.Background(), sqlite.CreateVirtualKeyParams{PolicyID: policy.ID, PolicyRevision: policy.Revision})
	if err != nil {
		t.Fatal(err)
	}
	var ready, draining atomic.Bool
	handler, closeComponents, err := composeHandlerWithFactory(prepared, &ready, &draining, nil, func(item topologyComponent) core.Component {
		if item.Kind == core.ComponentAdapter {
			return adapter.NewAdapter()
		}
		if item.ID == "upstream" {
			// Only this no-send candidate synthesizes RetrySafe; upstream-b is the production child.
			return codexSafeRejectionConnector{Connector: codex.NewConnector()}
		}
		t.Fatalf("unexpected in-process component %q", item.ID)
		return nil
	})
	if err != nil {
		_ = prepared.runtimeDB.Close()
		_ = prepared.processLock.Close()
		t.Fatal(err)
	}
	ready.Store(true)
	defer func() {
		_ = closeComponents(context.Background())
		_ = prepared.runtimeDB.Close()
		_ = prepared.processLock.Close()
	}()
	gateway := httptest.NewServer(handler)
	defer gateway.Close()
	body := []byte(`{"model":"model-a","input":"safe-retry-fixture"}`)
	req, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/responses", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+issued.Secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := gateway.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	responseBody, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil || resp.StatusCode != http.StatusOK || !bytes.Contains(responseBody, []byte("fallback-response")) {
		t.Fatalf("safe pre-send fallback response status=%d err=%v body=%q", resp.StatusCode, readErr, responseBody)
	}
	capturedBody := <-capturedBodies
	if got := sent.Load(); got != 1 || !bytes.Equal(capturedBody, body) {
		t.Fatalf("external secondary deliveries=%d body=%q, want one byte-preserved delivery %q", got, capturedBody, body)
	}
	ledger := sqlite.NewLedger(prepared.runtimeDB)
	var rows []sqlite.RequestUsage
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rows, err = ledger.QueryRequests(context.Background(), sqlite.RequestFilter{Model: "model-a", Limit: 2})
		settled := err == nil && len(rows) == 1 && len(rows[0].Attempts) == 2
		for _, attempt := range rows[0].Attempts {
			settled = settled && attempt.Reservation.State != "held"
		}
		if settled {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil || len(rows) != 1 || len(rows[0].Attempts) != 2 {
		t.Fatalf("safe fallback ledger=%+v err=%v, want one request/two attempts", rows, err)
	}
	seen := make(map[int64]bool, 2)
	var firstAttempt, secondAttempt *sqlite.AttemptUsage
	for _, attempt := range rows[0].Attempts {
		seen[attempt.Attempt.Ordinal] = true
		if attempt.Reservation.State == "held" || attempt.Reservation.ReconciledAt == nil {
			t.Fatalf("fallback attempt reservation not finalized: %+v", attempt)
		}
		copy := attempt
		switch attempt.Attempt.Ordinal {
		case 1:
			firstAttempt = &copy
		case 2:
			secondAttempt = &copy
		}
	}
	if !seen[1] || !seen[2] || firstAttempt == nil || secondAttempt == nil || firstAttempt.Attempt.AccountID != "account-a" || secondAttempt.Attempt.AccountID != "account-b" {
		t.Fatalf("fallback did not preserve sequential distinct candidate attempts: %+v", rows[0].Attempts)
	}
	if firstAttempt.Attempt.State != "failed" || firstAttempt.Reservation.State != "conservative" || firstAttempt.Reservation.EffectiveCharge != 10 {
		t.Fatalf("safe failed attempt must settle its reserved estimate conservatively: %+v", firstAttempt)
	}
	if secondAttempt.Attempt.State != "succeeded" || secondAttempt.Usage == nil || secondAttempt.Usage.InputTokens == nil || *secondAttempt.Usage.InputTokens != 1 || secondAttempt.Usage.OutputTokens == nil || *secondAttempt.Usage.OutputTokens != 2 || secondAttempt.Reservation.State != "settled" || secondAttempt.Reservation.EffectiveCharge != 3 {
		t.Fatalf("external fallback usage/reservation settlement: %+v", secondAttempt)
	}
}

func TestExternalCrashAccounting(t *testing.T) {
	for _, committed := range []bool{false, true} {
		name := "pre-head"
		if committed {
			name = "post-head"
		}
		t.Run(name, func(t *testing.T) {
			child := buildExternalResponsesChild(t)
			firstRequest := make(chan struct{})
			firstBytesSent := make(chan struct{})
			firstHandlerDone := make(chan struct{})
			var upstreamRequests atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				switch upstreamRequests.Add(1) {
				case 1:
					close(firstRequest)
					if committed {
						w.Header().Set("Content-Type", "text/event-stream")
						w.WriteHeader(http.StatusOK)
						_, _ = io.WriteString(w, "data: first\n\n")
						w.(http.Flusher).Flush()
						close(firstBytesSent)
					}
					<-r.Context().Done()
					close(firstHandlerDone)
				case 2:
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"restart-ok\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
				default:
					t.Errorf("unexpected upstream replay/request %d", upstreamRequests.Load())
					w.WriteHeader(http.StatusInternalServerError)
				}
			}))
			defer upstream.Close()
			_, dbPath, keyPath := protectedFixture(t)
			t.Setenv("PROTECTED_TEST_CREDENTIAL", "synthetic")
			c := fixtureProtectedConfig(dbPath, keyPath, "127.0.0.1:0")
			c.protected.Connectors[0].Settings.BaseURL = upstream.URL + "/v1/responses"
			c.protected.Connectors[0].Settings.Capabilities = map[string]string{"llm.streaming": "supported"}
			firstWorkDir, secondWorkDir := t.TempDir(), t.TempDir()
			c.protected.Connectors[0].Process = &protectedProcess{Executable: child, WorkingDirectory: firstWorkDir}
			second := c.protected.Connectors[0]
			second.ID = "upstream-b"
			second.Settings.CredentialEnv = "PROTECTED_TEST_CREDENTIAL_B"
			second.Process = &protectedProcess{Executable: child, WorkingDirectory: secondWorkDir}
			c.protected.Connectors = append(c.protected.Connectors, second)
			c.protected.Routes[0].Targets = []routeTarget{{Connector: "upstream", Account: "account-a"}, {Connector: "upstream-b", Account: "account-b"}}
			maxAttempts := 2
			c.protected.Routes[0].Retry = &retryConfig{MaxAttempts: &maxAttempts, Deadline: "5s"}
			seedExternalFallbackAccount(t, dbPath, keyPath)
			prepared, err := prepareProtectedConfig(context.Background(), c)
			if err != nil {
				t.Fatal(err)
			}
			issued, err := sqlite.NewVirtualKeys(prepared.runtimeDB).Create(context.Background(), sqlite.CreateVirtualKeyParams{PolicyID: "policy-id-a", PolicyRevision: 1})
			if err != nil {
				t.Fatal(err)
			}
			before := directChildPIDs(t)
			processDirsBefore := childProcessDirectories(t)
			var ready, draining atomic.Bool
			handler, closeComponents, err := composeHandler(prepared, &ready, &draining, nil)
			if err != nil {
				t.Fatal(err)
			}
			ready.Store(true)
			defer func() {
				_ = closeComponents(context.Background())
				_ = prepared.runtimeDB.Close()
				_ = prepared.processLock.Close()
			}()
			// Composition launches the child; identify it before sending the request.
			pid := waitChildInDirectory(t, before, firstWorkDir)
			secondPID := waitChildInDirectory(t, before, secondWorkDir)
			oldSocket := connectorSocketPath(t, pid)
			gateway := httptest.NewServer(handler)
			knownPIDs := []int{pid, secondPID}
			t.Cleanup(func() {
				gateway.Close()
				for childPID := range directChildPIDs(t) {
					cwd, _ := os.Readlink(fmt.Sprintf("/proc/%d/cwd", childPID))
					if cwd == firstWorkDir || cwd == secondWorkDir {
						knownPIDs = append(knownPIDs, childPID)
					}
				}
				if err := closeComponents(context.Background()); err != nil {
					t.Errorf("close recovered external gateway: %v", err)
				}
				for _, childPID := range knownPIDs {
					if err := syscall.Kill(-childPID, 0); !errors.Is(err, syscall.ESRCH) {
						t.Errorf("external process group %d remains after close: %v", childPID, err)
					}
				}
				if after := childProcessDirectories(t); hasNewChildProcessDirectories(processDirsBefore, after) {
					t.Errorf("external child socket directories leaked: before=%v after=%v", processDirsBefore, after)
				}
				_ = prepared.runtimeDB.Close()
				_ = prepared.processLock.Close()
			})
			body := []byte(`{"model":"model-a","stream":true,"input":"fixture"}`)
			req, _ := http.NewRequest(http.MethodPost, gateway.URL+"/v1/responses", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+issued.Secret)
			req.Header.Set("Content-Type", "application/json")
			headRead := make(chan struct{}, 1)
			response := make(chan struct {
				status  int
				readErr error
				body    []byte
			}, 1)
			go func() {
				resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
				if err != nil {
					response <- struct {
						status  int
						readErr error
						body    []byte
					}{readErr: err}
					return
				}
				var first []byte
				if committed {
					one := make([]byte, len("data: first\n\n"))
					_, err = io.ReadFull(resp.Body, one)
					if err == nil {
						first = one
						headRead <- struct{}{}
					}
				}
				body, readErr := io.ReadAll(resp.Body)
				body = append(first, body...)
				_ = resp.Body.Close()
				if err != nil {
					readErr = err
				}
				response <- struct {
					status  int
					readErr error
					body    []byte
				}{status: resp.StatusCode, readErr: readErr, body: body}
			}()
			if committed {
				select {
				case <-firstBytesSent:
				case <-time.After(5 * time.Second):
					t.Fatal("fixture did not send committed body")
				}
				select {
				case <-headRead:
				case <-time.After(5 * time.Second):
					t.Fatal("gateway did not commit and deliver first response bytes")
				}
			} else {
				select {
				case <-firstRequest:
				case <-time.After(5 * time.Second):
					t.Fatal("fixture did not receive ambiguous request")
				}
			}
			if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
				t.Fatalf("kill external child: %v", err)
			}
			select {
			case got := <-response:
				if committed {
					if got.status != http.StatusOK || !bytes.Equal(got.body, []byte("data: first\n\n")) {
						t.Fatalf("post-Head crash must terminate the committed partial stream: %+v", got)
					}
				} else if got.status != http.StatusServiceUnavailable {
					t.Fatalf("pre-Head crash status=%d, want unavailable failure: %+v", got.status, got)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("gateway did not terminate crashed request")
			}
			select {
			case <-firstHandlerDone:
			case <-time.After(5 * time.Second):
				t.Fatal("cancelled loopback upstream handler leaked after child crash")
			}
			ledger := sqlite.NewLedger(prepared.runtimeDB)
			deadline := time.Now().Add(5 * time.Second)
			var attempts []sqlite.AttemptUsage
			var crashedRequestID string
			for time.Now().Before(deadline) {
				requests, queryErr := ledger.QueryRequests(context.Background(), sqlite.RequestFilter{Limit: 10})
				if queryErr == nil && len(requests) == 1 {
					crashedRequestID = requests[0].Request.ID
					attempts = requests[0].Attempts
					if len(attempts) == 1 && attempts[0].Reservation.State != "held" {
						break
					}
				}
				time.Sleep(10 * time.Millisecond)
			}
			if len(attempts) != 1 || attempts[0].Usage == nil || attempts[0].Usage.InputTokens != nil || attempts[0].Usage.OutputTokens != nil || attempts[0].Reservation.State == "held" {
				t.Fatalf("crash must finalize once with unknown usage and no held reservation: %+v", attempts)
			}
			if attempts[0].Attempt.State == "succeeded" {
				t.Fatalf("crash was recorded as success: %+v", attempts[0])
			}
			wantState := "failed"
			if committed {
				wantState = "interrupted"
			}
			if attempts[0].Attempt.State != wantState || attempts[0].Attempt.Committed != committed {
				t.Fatalf("crashed request state/commit=%q/%t, want %q/%t", attempts[0].Attempt.State, attempts[0].Attempt.Committed, wantState, committed)
			}
			if got := upstreamRequests.Load(); got != 1 {
				t.Fatalf("ambiguous delivery was implicitly replayed %d times", got-1)
			}
			if attempts[0].Reservation.EffectiveCharge != 10 || attempts[0].Reservation.State != "conservative" {
				t.Fatalf("unknown usage must retain exactly the conservative reservation: %+v", attempts[0].Reservation)
			}
			crashedAttemptID := attempts[0].Attempt.ID
			deadline = time.Now().Add(5 * time.Second)
			recovered := false
			for time.Now().Before(deadline) {
				for childPID := range directChildPIDs(t) {
					cwd, _ := os.Readlink(fmt.Sprintf("/proc/%d/cwd", childPID))
					if childPID != pid && cwd == firstWorkDir {
						recovered = true
						knownPIDs = append(knownPIDs, childPID)
					}
				}
				if recovered {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			if !recovered {
				t.Fatal("supervisor did not restart the child after the crash")
			}
			var newPID int
			for childPID := range directChildPIDs(t) {
				cwd, _ := os.Readlink(fmt.Sprintf("/proc/%d/cwd", childPID))
				if childPID != pid && cwd == firstWorkDir {
					newPID = childPID
				}
			}
			if newPID == 0 {
				t.Fatal("replacement child process disappeared before generation check")
			}
			newSocket := connectorSocketPath(t, newPID)
			if newSocket == oldSocket {
				t.Fatalf("restart did not publish a fresh process generation: old pid/socket=%d/%q new=%d/%q", pid, oldSocket, newPID, newSocket)
			}
			// The child PID appears before its IPC handshake/readiness completes.
			time.Sleep(250 * time.Millisecond)
			// A distinct second client request succeeds through the recovered process;
			// the old ambiguous request remains one upstream delivery and one attempt.
			secondPayload := []byte(`{"model":"model-a","stream":true,"input":"fixture-after-restart"}`)
			secondReq, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/responses", bytes.NewReader(secondPayload))
			if err != nil {
				t.Fatal(err)
			}
			secondReq.Header.Set("Authorization", "Bearer "+issued.Secret)
			secondReq.Header.Set("Content-Type", "application/json")
			secondResp, err := (&http.Client{Timeout: 10 * time.Second}).Do(secondReq)
			if err != nil {
				t.Fatalf("new request after child restart: %v", err)
			}
			secondBody, readErr := io.ReadAll(secondResp.Body)
			_ = secondResp.Body.Close()
			if readErr != nil || secondResp.StatusCode != http.StatusOK || !bytes.Contains(secondBody, []byte("response.completed")) {
				t.Fatalf("recovered child request status=%d err=%v body=%q", secondResp.StatusCode, readErr, secondBody)
			}
			if got := upstreamRequests.Load(); got != 2 {
				t.Fatalf("upstream delivery count=%d, want crashed request plus one new request", got)
			}
			var requests []sqlite.RequestUsage
			deadline = time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				requests, err = ledger.QueryRequests(context.Background(), sqlite.RequestFilter{Limit: 10})
				settled := err == nil && len(requests) == 2
				for _, request := range requests {
					settled = settled && len(request.Attempts) == 1 && request.Attempts[0].Reservation.State != "held"
				}
				if settled {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err != nil || len(requests) != 2 {
				t.Fatalf("post-restart durable requests=%+v err=%v", requests, err)
			}
			var newRequest *sqlite.RequestUsage
			for i := range requests {
				request := &requests[i]
				if len(request.Attempts) != 1 || request.Attempts[0].Attempt.Ordinal != 1 || request.Attempts[0].Reservation.State == "held" {
					t.Fatalf("each client request must have one finalized attempt: %+v", *request)
				}
				if request.Attempts[0].Attempt.AccountID != "account-a" {
					t.Fatalf("new request did not use the recovered primary external target: %+v", request.Attempts[0].Attempt)
				}
				if request.Request.ID == crashedRequestID {
					if request.Attempts[0].Attempt.ID != crashedAttemptID || request.Attempts[0].Usage == nil || request.Attempts[0].Usage.InputTokens != nil || request.Attempts[0].Usage.OutputTokens != nil || request.Attempts[0].Reservation.State != "conservative" || request.Attempts[0].Reservation.EffectiveCharge != 10 {
						t.Fatalf("crashed request accounting changed or replayed: %+v", request.Attempts[0])
					}
				} else {
					newRequest = request
				}
			}
			if newRequest == nil || newRequest.Request.ID == crashedRequestID {
				t.Fatalf("missing distinct post-restart request: %+v", requests)
			}
			attempt := newRequest.Attempts[0]
			if attempt.Attempt.State != "succeeded" || attempt.Usage == nil || attempt.Usage.InputTokens == nil || *attempt.Usage.InputTokens != 1 || attempt.Usage.OutputTokens == nil || *attempt.Usage.OutputTokens != 1 || attempt.Reservation.State != "settled" || attempt.Reservation.EffectiveCharge != 2 || attempt.Reservation.ReconciledAt == nil {
				t.Fatalf("new request usage/settlement after fresh generation = %+v", attempt)
			}
		})
	}
}

func waitChildInDirectory(t *testing.T, before map[int]struct{}, dir string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for pid := range directChildPIDs(t) {
			cwd, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
			if _, ok := before[pid]; !ok && err == nil && cwd == dir {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("external child process in %q did not start", dir)
	return 0
}

func connectorSocketPath(t *testing.T, pid int) string {
	t.Helper()
	env, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		t.Fatalf("read child %d environment: %v", pid, err)
	}
	for entry := range bytes.SplitSeq(env, []byte{0}) {
		key, value, ok := bytes.Cut(entry, []byte("="))
		if ok && string(key) == "PESTIROUTE_CONNECTOR_SOCKET" {
			return string(value)
		}
	}
	t.Fatalf("child %d has no connector socket identity", pid)
	return ""
}

func seedExternalFallbackAccount(t *testing.T, dbPath, keyPath string) {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := sqlite.NewAccounts(db).Create(ctx, sqlite.Account{ID: "account-b", Connector: "upstream-b", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	policies := sqlite.NewKeyPolicies(db)
	policy, err := policies.GetLatest(ctx, "policy-id-a")
	if err != nil {
		t.Fatal(err)
	}
	_, err = policies.Update(ctx, policy.ID, policy.Revision, sqlite.UpdateKeyPolicyParams{Enabled: true, Models: policy.Models, Connectors: []string{"upstream", "upstream-b"}, RPM: policy.RPM, TPM: policy.TPM})
	if err != nil {
		t.Fatal(err)
	}
	master, err := secure.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := secure.Seal(master, 2, "test-v1", "credentials", "PROTECTED_TEST_CREDENTIAL_B", "account-b", []byte("synthetic"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.NewCredentials(db).Create(ctx, sqlite.Credential{ID: "PROTECTED_TEST_CREDENTIAL_B", AccountID: "account-b", FormatVersion: envelope.FormatVersion, KeyVersion: envelope.KeyVersion, Nonce: envelope.Nonce, Ciphertext: envelope.Ciphertext}); err != nil {
		t.Fatal(err)
	}
}

func buildExternalResponsesChild(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Dir(filepath.Dir(filepath.Dir(file)))
	child := filepath.Join(t.TempDir(), "responses-connector-child")
	build := exec.Command("go", "build", "-o", child, "./cmd/responses-connector-child")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build production child: %v\n%s", err, output)
	}
	return child
}

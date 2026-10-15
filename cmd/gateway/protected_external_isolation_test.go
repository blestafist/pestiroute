//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	secure "github.com/blestafist/pestiroute/internal/crypto"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

func TestM6ProtectedExternalAccountIsolation(t *testing.T) {
	_, dbPath, keyPath := protectedFixture(t)
	ctx := context.Background()
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.NewAccounts(db).Create(ctx, sqlite.Account{ID: "account-b", Connector: "upstream-b", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	policies := sqlite.NewKeyPolicies(db)
	policyA, err := policies.GetLatest(ctx, "policy-id-a")
	if err != nil {
		t.Fatal(err)
	}
	policyA, err = policies.Update(ctx, policyA.ID, policyA.Revision, sqlite.UpdateKeyPolicyParams{Enabled: true, Models: []string{"model-a"}, Connectors: []string{"upstream"}, RPM: 20, TPM: 1000})
	if err != nil {
		t.Fatal(err)
	}
	policyB, err := policies.Create(ctx, sqlite.CreateKeyPolicyParams{ID: "policy-b", Enabled: true, Models: []string{"model-b"}, Connectors: []string{"upstream-b"}, RPM: 20, TPM: 1000})
	if err != nil {
		t.Fatal(err)
	}
	master, err := secure.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := secure.Seal(master, 2, "test-v1", "credentials", "PROTECTED_TEST_CREDENTIAL_B", "account-b", []byte("synthetic-b"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.NewCredentials(db).Create(ctx, sqlite.Credential{ID: "PROTECTED_TEST_CREDENTIAL_B", AccountID: "account-b", FormatVersion: envelope.FormatVersion, KeyVersion: envelope.KeyVersion, Nonce: envelope.Nonce, Ciphertext: envelope.Ciphertext}); err != nil {
		t.Fatal(err)
	}
	keys := sqlite.NewVirtualKeys(db)
	keyA, err := keys.Create(ctx, sqlite.CreateVirtualKeyParams{PolicyID: policyA.ID, PolicyRevision: policyA.Revision})
	if err != nil {
		t.Fatal(err)
	}
	keyB, err := keys.Create(ctx, sqlite.CreateVirtualKeyParams{PolicyID: policyB.ID, PolicyRevision: policyB.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	type delivery struct{ auth, body string }
	deliveries := make(chan delivery, 4)
	arrived := make(chan string, 4)
	phaseArrived := make(chan string, 2)
	initialRelease, phaseARelease, phaseBRelease := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var initialReleaseOnce, phaseAReleaseOnce, phaseBReleaseOnce sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			t.Errorf("read upstream request: %v", readErr)
			return
		}
		auth := r.Header.Get("Authorization")
		marker := "account-a"
		if auth == "Bearer synthetic-b" {
			marker = "account-b"
		} else if auth != "Bearer synthetic" {
			t.Errorf("unexpected upstream authorization value")
		}
		deliveries <- delivery{auth: auth, body: string(body)}
		arrived <- marker
		gate := initialRelease
		switch {
		case strings.Contains(string(body), "request-stale-epoch-frame"):
			gate = phaseARelease
			phaseArrived <- "account-a"
		case strings.Contains(string(body), "request-during-stale-frame"):
			gate = phaseBRelease
			phaseArrived <- "account-b"
		}
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":%q,"status":"completed","usage":{"input_tokens":%d,"output_tokens":%d}}`, marker, 3, 4)
	}))
	defer upstream.Close()
	child := buildExternalIsolationChild(t)
	workA, workB := t.TempDir(), t.TempDir()
	identityFile, auditFile := filepath.Join(t.TempDir(), "identities.jsonl"), filepath.Join(t.TempDir(), "audit.txt")
	c := fixtureProtectedConfig(dbPath, keyPath, "127.0.0.1:0")
	c.protected.Connectors[0].Settings.BaseURL = upstream.URL + "/v1/responses"
	c.protected.Connectors[0].Settings.StreamIdleTimeout = "10s"
	c.protected.Connectors[0].Settings.ResponseHeaderTimeout = "10s"
	c.protected.Connectors[0].Process = &protectedProcess{Executable: child, Args: []string{identityFile, auditFile, "PROTECTED_TEST_CREDENTIAL", "PROTECTED_TEST_CREDENTIAL_B"}, WorkingDirectory: workA}
	second := c.protected.Connectors[0]
	second.ID = "upstream-b"
	second.Settings.CredentialEnv = "PROTECTED_TEST_CREDENTIAL_B"
	second.Process = &protectedProcess{Executable: child, Args: []string{identityFile, auditFile, "PROTECTED_TEST_CREDENTIAL_B", "PROTECTED_TEST_CREDENTIAL"}, WorkingDirectory: workB}
	c.protected.Connectors = append(c.protected.Connectors, second)
	c.protected.Routes = []protectedRoute{
		{ID: "route-a", Protocol: responsesProtocol, Mode: "native", Model: "model-a", Adapter: "pestiroute.responses.native", Policy: "account-a", Budget: routeBudget{UnknownEstimate: "reserve", ConservativeTokens: ptrInt64(10)}, Targets: []routeTarget{{Connector: "upstream", Account: "account-a"}}},
		{ID: "route-b", Protocol: responsesProtocol, Mode: "native", Model: "model-b", Adapter: "pestiroute.responses.native", Policy: "account-b", Budget: routeBudget{UnknownEstimate: "reserve", ConservativeTokens: ptrInt64(10)}, Targets: []routeTarget{{Connector: "upstream-b", Account: "account-b"}}},
	}
	c.protected.Policies = map[string]string{"account-a": policyA.ID, "account-b": policyB.ID}
	prepared, err := prepareProtectedConfig(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	prepared.logger = slog.New(slog.NewTextHandler(&logs, nil))
	var ready, draining atomic.Bool
	processDirsBefore := childProcessDirectories(t)
	parentChildren := directChildPIDs(t)
	handler, closeComponents, err := composeHandler(prepared, &ready, &draining, nil)
	if err != nil {
		_ = prepared.runtimeDB.Close()
		_ = prepared.processLock.Close()
		t.Fatal(err)
	}
	ready.Store(true)
	pidA := waitChildInDirectory(t, parentChildren, workA)
	pidB := waitChildInDirectory(t, parentChildren, workB)
	generationPIDs := []int{pidA, pidB}
	server := httptest.NewServer(handler)
	t.Cleanup(func() {
		initialReleaseOnce.Do(func() { close(initialRelease) })
		phaseAReleaseOnce.Do(func() { close(phaseARelease) })
		phaseBReleaseOnce.Do(func() { close(phaseBRelease) })
		server.Close()
		known := make(map[int]bool, len(generationPIDs))
		for _, pid := range generationPIDs {
			known[pid] = true
		}
		for pid := range directChildPIDs(t) {
			cwd, _ := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
			if (cwd == workA || cwd == workB) && !known[pid] {
				generationPIDs = append(generationPIDs, pid)
				known[pid] = true
			}
		}
		if err := closeComponents(context.Background()); err != nil {
			t.Errorf("close protected external gateway: %v", err)
		}
		for _, pid := range generationPIDs {
			if !waitExternalChildReaped(pid, 5*time.Second) {
				t.Errorf("external child process group/process %d remains after gateway close", pid)
			}
		}
		deadline := time.Now().Add(5 * time.Second)
		for hasNewChildProcessDirectories(processDirsBefore, childProcessDirectories(t)) && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if after := childProcessDirectories(t); hasNewChildProcessDirectories(processDirsBefore, after) {
			t.Errorf("external child socket directories leaked: before=%v after=%v", processDirsBefore, after)
		}
		_ = prepared.runtimeDB.Close()
		_ = prepared.processLock.Close()
	})

	type result struct {
		name   string
		status int
		body   []byte
		err    error
	}
	results := make(chan result, 2)
	call := func(name, secret, model, input string) result {
		body := fmt.Appendf(nil, `{"model":%q,"input":%q}`, model, input)
		req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", bytes.NewReader(body))
		if err != nil {
			return result{name: name, err: err}
		}
		req.Header.Set("Authorization", "Bearer "+secret)
		req.Header.Set("Content-Type", "application/json")
		resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
		if err != nil {
			return result{name: name, err: err}
		}
		responseBody, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return result{name: name, status: resp.StatusCode, body: responseBody, err: err}
	}
	for _, request := range []struct{ name, secret, model, input string }{{"a", keyA.Secret, "model-a", "request-a"}, {"b", keyB.Secret, "model-b", "request-b"}} {
		go func() { results <- call(request.name, request.secret, request.model, request.input) }()
	}
	seen := map[string]bool{}
	for range 2 {
		select {
		case marker := <-arrived:
			if seen[marker] {
				t.Fatalf("duplicate upstream account delivery %q", marker)
			}
			seen[marker] = true
		case <-time.After(10 * time.Second):
			t.Fatal("both protected external account requests did not reach upstream concurrently")
		}
	}
	initialReleaseOnce.Do(func() { close(initialRelease) })
	for range 2 {
		got := <-results
		if got.err != nil || got.status != http.StatusOK {
			t.Fatalf("request %s status=%d err=%v body=%q", got.name, got.status, got.err, got.body)
		}
		want := "account-" + got.name
		if !strings.Contains(string(got.body), want) || strings.Contains(string(got.body), map[bool]string{true: "account-b", false: "account-a"}[got.name == "a"]) {
			t.Fatalf("request %s received cross-account response frame: %q", got.name, got.body)
		}
	}
	wantDeliveries := map[string]string{"Bearer synthetic": "request-a", "Bearer synthetic-b": "request-b"}
	for range 2 {
		got := <-deliveries
		if want := wantDeliveries[got.auth]; want == "" || !strings.Contains(got.body, want) {
			t.Fatalf("credential/request crossed accounts: authorization class=%q request=%q", map[string]string{"Bearer synthetic": "a", "Bearer synthetic-b": "b"}[got.auth], got.body)
		}
		delete(wantDeliveries, got.auth)
	}
	if len(wantDeliveries) != 0 {
		t.Fatalf("missing scoped deliveries: %v", wantDeliveries)
	}
	initialDeadline := time.Now().Add(5 * time.Second)
	initialSettled := false
	for time.Now().Before(initialDeadline) {
		rows, queryErr := sqlite.NewLedger(prepared.runtimeDB).QueryRequests(ctx, sqlite.RequestFilter{Limit: 10})
		settled := queryErr == nil && len(rows) == 2
		for _, row := range rows {
			settled = settled && len(row.Attempts) == 1 && row.Attempts[0].Attempt.State == "succeeded" && row.Attempts[0].Reservation.State == "settled"
		}
		if settled {
			initialSettled = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !initialSettled {
		t.Fatal("initial concurrent account usage did not settle before epoch restart")
	}
	oldEpochPID := pidA
	if err := syscall.Kill(oldEpochPID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill first account child epoch: %v", err)
	}
	pidA = waitChildInDirectory(t, parentChildren, workA)
	if !waitExternalChildReaped(oldEpochPID, 5*time.Second) {
		t.Fatalf("stale child process group/process %d survived replacement", oldEpochPID)
	}
	generationPIDs = append(generationPIDs, pidA)
	time.Sleep(300 * time.Millisecond)
	staleGenerationPID := pidA
	staleChildrenBefore := directChildPIDs(t)
	staleFrames := make(chan result, 1)
	liveRequests := make(chan result, 1)
	go func() { staleFrames <- call("a", keyA.Secret, "model-a", "request-stale-epoch-frame") }()
	go func() { liveRequests <- call("b", keyB.Secret, "model-b", "request-during-stale-frame") }()
	phaseSeen := map[string]bool{}
	for range 2 {
		select {
		case account := <-phaseArrived:
			phaseSeen[account] = true
		case <-time.After(10 * time.Second):
			t.Fatal("stale-frame phase did not hold both account requests at the upstream barrier")
		}
	}
	if !phaseSeen["account-a"] || !phaseSeen["account-b"] {
		t.Fatalf("stale-frame barrier arrivals=%v, want both active accounts", phaseSeen)
	}
	phaseAReleaseOnce.Do(func() { close(phaseARelease) })
	staleFrame := <-staleFrames
	if staleFrame.err != nil || staleFrame.status == http.StatusOK {
		t.Fatalf("stale epoch frame was accepted: status=%d err=%v body=%q", staleFrame.status, staleFrame.err, staleFrame.body)
	}
	pidA = waitChildInDirectory(t, staleChildrenBefore, workA)
	if !waitExternalChildReaped(staleGenerationPID, 5*time.Second) {
		t.Fatalf("stale-frame child process group/process %d survived replacement", staleGenerationPID)
	}
	generationPIDs = append(generationPIDs, pidA)
	time.Sleep(time.Second) // the supervisor publishes the process before handshake and Init complete.
	select {
	case got := <-liveRequests:
		t.Fatalf("account B completed before its phase gate opened: status=%d body=%q", got.status, got.body)
	case <-time.After(100 * time.Millisecond):
	}
	phaseBReleaseOnce.Do(func() { close(phaseBRelease) })
	live := <-liveRequests
	if live.err != nil || live.status != http.StatusOK || !bytes.Contains(live.body, []byte("account-b")) {
		t.Fatalf("stale callback/frame disturbed concurrent account B execution: status=%d err=%v body=%q", live.status, live.err, live.body)
	}
	currentFrame := call("a", keyA.Secret, "model-a", "request-after-stale-frame")
	if currentFrame.err != nil || currentFrame.status != http.StatusOK || !bytes.Contains(currentFrame.body, []byte("account-a")) {
		t.Fatalf("valid current-epoch invocation disrupted by stale callback/frame: status=%d err=%v body=%q", currentFrame.status, currentFrame.err, currentFrame.body)
	}
	for range 3 {
		got := <-deliveries
		if strings.Contains(got.body, "request-during-stale-frame") {
			if got.auth != "Bearer synthetic-b" {
				t.Fatalf("cross-account credential during stale frame race: %q", got.auth)
			}
		} else if got.auth != "Bearer synthetic" || (!strings.Contains(got.body, "request-stale-epoch-frame") && !strings.Contains(got.body, "request-after-stale-frame")) {
			t.Fatalf("post-restart request crossed account boundary: %q / %q", got.auth, got.body)
		}
	}
	audit, err := os.ReadFile(auditFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"foreign-credential-denied", "spoofed-invocation-denied", "stale-epoch-denied", "stale-frame-injected"} {
		if !bytes.Contains(audit, []byte(required)) {
			t.Fatalf("child did not record fail-closed result %q; audit=%q", required, audit)
		}
	}
	for _, forbidden := range []string{"foreign-credential-accepted", "spoofed-invocation-accepted", "stale-epoch-accepted"} {
		if bytes.Contains(audit, []byte(forbidden)) {
			t.Fatalf("child authority violation recorded %q; audit=%q", forbidden, audit)
		}
	}
	for _, secret := range []string{"synthetic", "synthetic-b", keyA.Secret, keyB.Secret} {
		if bytes.Contains(logs.Bytes(), []byte(secret)) {
			t.Fatal("credential or virtual-key secret leaked to protected gateway logs")
		}
		if bytes.Contains(audit, []byte(secret)) {
			t.Fatal("credential or virtual-key secret leaked to test-child audit")
		}
	}
	stored, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"synthetic", "synthetic-b", keyA.Secret, keyB.Secret} {
		if bytes.Contains(stored, []byte(secret)) {
			t.Fatal("credential or virtual-key secret was stored in plaintext")
		}
	}
	ledger := sqlite.NewLedger(prepared.runtimeDB)
	deadline := time.Now().Add(5 * time.Second)
	var finalRows []sqlite.RequestUsage
	for time.Now().Before(deadline) {
		rows, queryErr := ledger.QueryRequests(ctx, sqlite.RequestFilter{Limit: 10})
		finalRows = rows
		if queryErr == nil && len(rows) == 5 {
			accounts := map[string]int{"account-a": 3, "account-b": 2}
			failed := 0
			settled := true
			for _, row := range rows {
				if len(row.Attempts) != 1 || row.Attempts[0].Reservation.State == "held" {
					settled = false
					break
				}
				attempt := row.Attempts[0]
				if accounts[attempt.Attempt.AccountID] == 0 {
					t.Fatalf("usage attributed to unexpected account: %+v", attempt)
				}
				accounts[attempt.Attempt.AccountID]--
				if attempt.Attempt.State == "succeeded" {
					if attempt.Reservation.State != "settled" || attempt.Usage == nil || attempt.Usage.InputTokens == nil || *attempt.Usage.InputTokens != 3 || attempt.Usage.OutputTokens == nil || *attempt.Usage.OutputTokens != 4 {
						t.Fatalf("misattributed or unsettled successful usage: %+v", attempt)
					}
				} else {
					failed++
					if attempt.Attempt.AccountID != "account-a" || attempt.Reservation.State != "conservative" || attempt.Usage == nil || attempt.Usage.InputTokens != nil || attempt.Usage.OutputTokens != nil {
						t.Fatalf("stale frame accounting was not finalized conservatively: %+v", attempt)
					}
				}
			}
			if settled && accounts["account-a"] == 0 && accounts["account-b"] == 0 && failed == 1 {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("per-account ledger usage did not settle: %+v", finalRows)
}

func buildExternalIsolationChild(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Dir(filepath.Dir(filepath.Dir(file)))
	child := filepath.Join(t.TempDir(), "isolation-connector-child")
	build := exec.Command("go", "build", "-o", child, "./cmd/gateway/testdata/isolation-child")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build test isolation child: %v\n%s", err, output)
	}
	return child
}

func waitExternalChildReaped(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		groupGone := errors.Is(syscall.Kill(-pid, 0), syscall.ESRCH)
		_, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
		if groupGone && errors.Is(err, os.ErrNotExist) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.Is(syscall.Kill(-pid, 0), syscall.ESRCH) && errors.Is(statProcess(pid), os.ErrNotExist)
}

func statProcess(pid int) error {
	_, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
	return err
}

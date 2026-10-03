package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

func TestAbruptRestartAccounting(t *testing.T) {
	t.Setenv("PROTECTED_TEST_CREDENTIAL", "synthetic")
	dir, dbPath, keyPath := protectedFixture(t)
	binary := buildGatewayBinary(t)
	if _, err := runAdminProcess(t, binary, dbPath, keyPath, "policy", "update", "policy-id-a", "--expected-revision", "1", "--tpm", "20"); err != nil {
		t.Fatal(err)
	}
	keyOutput, err := runAdminProcess(t, binary, dbPath, keyPath, "key", "create", "--policy", "policy-id-a")
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.TrimPrefix(strings.Fields(keyOutput)[0], "secret=")
	if !strings.HasPrefix(secret, "prv_") {
		t.Fatalf("invalid fixture key output %q", keyOutput)
	}
	keyID := ""
	for _, field := range strings.Fields(keyOutput)[1:] {
		name, value, ok := strings.Cut(field, "=")
		if ok && name == "id" {
			keyID = value
		}
	}
	if keyID == "" {
		t.Fatalf("missing fixture key ID in %q", keyOutput)
	}

	var upstreamCalls atomic.Int32
	var upstreamCredential atomic.Value
	upstreamEntered := make(chan struct{}, 2)
	upstreamFinished := make(chan struct{}, 2)
	activeGate, settlementGate := make(chan struct{}), make(chan struct{})
	var activeGateOnce, settlementGateOnce sync.Once
	releaseActive := func() { activeGateOnce.Do(func() { close(activeGate) }) }
	releaseSettlement := func() { settlementGateOnce.Do(func() { close(settlementGate) }) }
	defer releaseActive()
	defer releaseSettlement()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCredential.Store(r.Header.Get("Authorization"))
		switch upstreamCalls.Add(1) {
		case 1:
			upstreamEntered <- struct{}{}
			<-activeGate
		case 2:
			upstreamEntered <- struct{}{}
			<-settlementGate
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"completed"}`)
		upstreamFinished <- struct{}{}
	}))
	defer upstream.Close()
	address := freeTCPAddress(t)
	configPath := writeProtectedYAML(t, dbPath, keyPath, address, upstream.URL+"/v1/responses", "1s")

	// The test-binary child commits real SQLite admission, reports its exact IDs,
	// then blocks inside the injected store before Core can record intent.
	preIntent := startPreIntentCrashProcess(t, configPath)
	waitProcessReady(t, address, preIntent.cmd)
	preIntentResponse := sendRestartRequest(address, secret)
	markerCh := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(preIntent.stdout).ReadString('\n')
		markerCh <- line
	}()
	var marker string
	select {
	case marker = <-markerCh:
	case <-time.After(5 * time.Second):
		t.Fatal("post-Admit helper did not report its boundary")
	}
	fields := strings.Fields(marker)
	if len(fields) != 3 || fields[0] != "ADMITTED" {
		t.Fatalf("invalid post-Admit marker %q", marker)
	}
	undispatchedRequestID, undispatchedAttemptID := fields[1], fields[2]
	if err := preIntent.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = preIntent.cmd.Wait()
	_ = preIntent.stdout.Close()
	select {
	case <-preIntentResponse:
	case <-time.After(2 * time.Second):
		t.Fatal("pre-intent client did not observe child process crash")
	}
	if upstreamCalls.Load() != 0 {
		t.Fatalf("pre-intent crash replayed upstream request: calls=%d", upstreamCalls.Load())
	}
	proc := startGatewayProcess(t, binary, configPath)
	waitProcessReady(t, address, proc)
	assertUndispatchedRecovered(t, dbPath, undispatchedRequestID, undispatchedAttemptID)
	undispatchedCLI := readRestartCLIRequests(t, binary, dbPath, keyPath, keyID)
	assertRestartCLIRequests(t, undispatchedCLI.Rows, keyID, undispatchedRequestID, 0)
	if upstreamCalls.Load() != 0 {
		t.Fatalf("recovery replayed undispatched work: %d upstream calls", upstreamCalls.Load())
	}
	if err := proc.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = proc.Wait()

	// Crash an actual request while dispatch is active at the upstream.
	proc = startGatewayProcess(t, binary, configPath)
	waitProcessReady(t, address, proc)
	activeRequestDone := sendRestartRequest(address, secret)
	select {
	case <-upstreamEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("active dispatched request did not reach upstream")
	}
	activeRequestID, activeAttemptID := assertInFlightBoundary(t, dbPath, keyID, undispatchedRequestID)
	if err := proc.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = proc.Wait()
	releaseActive()
	select {
	case <-activeRequestDone:
	case <-time.After(2 * time.Second):
		t.Fatal("active request client did not observe abrupt termination")
	}
	proc = startGatewayProcess(t, binary, configPath)
	waitProcessReady(t, address, proc)
	assertRecoveredAttempt(t, dbPath, activeRequestID, activeAttemptID)
	activeSnapshot := readRestartCLIRequests(t, binary, dbPath, keyPath, keyID)
	assertRestartCLIRequests(t, activeSnapshot.Rows, keyID, undispatchedRequestID, 1)
	if err := proc.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = proc.Wait()

	// Crash after upstream completion while a separate SQLite writer holds
	// terminal settlement off.
	proc = startGatewayProcess(t, binary, configPath)
	waitProcessReady(t, address, proc)
	postRestartActive := readRestartCLIRequests(t, binary, dbPath, keyPath, keyID)
	assertRestartCLIRequests(t, postRestartActive.Rows, keyID, undispatchedRequestID, 1)
	if postRestartActive.JSON != activeSnapshot.JSON {
		t.Fatalf("restart changed exact active-crash snapshot:\nbefore=%s\nafter=%s", activeSnapshot.JSON, postRestartActive.JSON)
	}
	requestDone := sendRestartRequest(address, secret)
	select {
	case <-upstreamEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("second dispatched request did not reach upstream")
	}
	lockDB, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := lockDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	completedRequestID, completedAttemptID := assertInFlightBoundary(t, dbPath, keyID, undispatchedRequestID, activeRequestID)
	releaseSettlement()
	select {
	case <-upstreamFinished:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream did not finish before settlement crash")
	}
	time.Sleep(100 * time.Millisecond)
	assertHeldIntent(t, dbPath, completedRequestID, completedAttemptID)
	if err := proc.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = proc.Wait()
	_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
	_ = conn.Close()
	_ = lockDB.Close()
	select {
	case <-requestDone:
	case <-time.After(2 * time.Second):
		t.Fatal("request client did not observe abrupt termination")
	}
	// Recovery must precede readiness and be idempotent over further restarts.
	var restartSnapshot string
	for i := 0; i < 2; i++ {
		proc = startGatewayProcess(t, binary, configPath)
		waitProcessReady(t, address, proc)
		if i == 0 {
			assertRecoveredAttempt(t, dbPath, completedRequestID, completedAttemptID)
		}
		currentSnapshot := readRestartCLIRequests(t, binary, dbPath, keyPath, keyID)
		assertRestartCLIRequests(t, currentSnapshot.Rows, keyID, undispatchedRequestID, 2)
		if i > 0 && restartSnapshot != currentSnapshot.JSON {
			t.Fatalf("repeated restart changed exact CLI snapshot:\nbefore=%s\nafter=%s", restartSnapshot, currentSnapshot.JSON)
		}
		restartSnapshot = currentSnapshot.JSON
		if err := proc.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		_ = proc.Wait()
	}
	if got := upstreamCalls.Load(); got != 2 {
		t.Fatalf("upstream calls after crash/restarts = %d, want exactly two", got)
	}
	if got := upstreamCredential.Load(); got != "Bearer synthetic" {
		t.Fatalf("upstream received unexpected credential %v", got)
	}
	summaryJSON, err := runAdminProcess(t, binary, dbPath, keyPath, "usage", "summary", "--key", keyID, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var summary struct {
		Requests        int64 `json:"requests"`
		Attempts        int64 `json:"attempts"`
		EstimatedTokens int64 `json:"estimated_tokens"`
		EffectiveCharge int64 `json:"effective_charge"`
	}
	if err := json.Unmarshal([]byte(summaryJSON), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Requests != 3 || summary.Attempts != 3 || summary.EstimatedTokens != 30 || summary.EffectiveCharge != 20 {
		t.Fatalf("recovered usage summary requests=%d attempts=%d estimated=%d charge=%d", summary.Requests, summary.Attempts, summary.EstimatedTokens, summary.EffectiveCharge)
	}

	// The persisted key, policy and encrypted account credential still support
	// a fresh request after recovery; the gateway process remains a real binary.
	proc = startGatewayProcess(t, binary, configPath)
	waitProcessReady(t, address, proc)
	resp, err := postRestartRequest(address, secret)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(string(body), `"code":"rate_limit_exceeded"`) {
		t.Fatalf("retained charges were not enforced after restart: status=%d body=%s", resp.StatusCode, body)
	}
	if got := upstreamCalls.Load(); got != 2 {
		t.Fatalf("rate-limited request reached upstream: calls=%d", got)
	}
	newKeyOutput, err := runAdminProcess(t, binary, dbPath, keyPath, "key", "create", "--policy", "policy-id-a")
	if err != nil {
		t.Fatal(err)
	}
	newSecret := strings.TrimPrefix(strings.Fields(newKeyOutput)[0], "secret=")
	resp, err = postRestartRequest(address, newSecret)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || upstreamCalls.Load() != 3 {
		t.Fatalf("new key with persisted policy failed: status=%d upstream calls=%d", resp.StatusCode, upstreamCalls.Load())
	}
	cancelProcess(t, proc)
	usage, err := runAdminProcess(t, binary, dbPath, keyPath, "usage", "attempts", "--json")
	if err != nil || strings.Contains(usage, secret) || strings.Contains(usage, newSecret) || strings.Contains(usage, "synthetic") {
		t.Fatalf("CLI output leaked fixture secret or failed: output=%s err=%v", usage, err)
	}
	for _, name := range []string{"gateway.db", "gateway.db-wal", "gateway.db-shm"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err == nil && (strings.Contains(string(data), secret) || strings.Contains(string(data), newSecret) || strings.Contains(string(data), "synthetic")) {
			t.Fatalf("SQLite file %s contains plaintext fixture secret", name)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "master.key")); err != nil {
		t.Fatal(err)
	}
}

func assertInFlightBoundary(t *testing.T, dbPath, keyID string, excluded ...string) (string, string) {
	t.Helper()
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := sqlite.NewLedger(db).QueryRequests(context.Background(), sqlite.RequestFilter{VirtualKeyID: keyID, Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	skip := map[string]bool{}
	for _, id := range excluded {
		skip[id] = true
	}
	var requestID, attemptID string
	for _, row := range rows {
		if skip[row.Request.ID] {
			continue
		}
		if requestID != "" || len(row.Attempts) != 1 {
			t.Fatalf("expected one exact in-flight request, got %+v", row)
		}
		requestID, attemptID = row.Request.ID, row.Attempts[0].Attempt.ID
		if row.Request.State != "admitted" || row.Attempts[0].Attempt.State != "intent" || row.Attempts[0].Reservation.State != "held" || row.Attempts[0].Reservation.EffectiveCharge != 0 {
			t.Fatalf("upstream request did not reach durable intent boundary: %+v", row)
		}
	}
	if requestID == "" || attemptID == "" {
		t.Fatal("exact in-flight request/attempt IDs not found")
	}
	return requestID, attemptID
}

func assertHeldIntent(t *testing.T, dbPath, requestID, attemptID string) {
	t.Helper()
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ledger := sqlite.NewLedger(db)
	request, reqErr := ledger.GetRequest(context.Background(), requestID)
	attempt, attErr := ledger.GetAttempt(context.Background(), attemptID)
	reservation, resErr := ledger.GetReservation(context.Background(), attemptID)
	_, usageErr := ledger.GetUsage(context.Background(), attemptID)
	if reqErr != nil || attErr != nil || resErr != nil || request.State != "admitted" || attempt.State != "intent" || reservation.State != "held" || reservation.EffectiveCharge != 0 || !errors.Is(usageErr, sqlite.ErrLedgerNotFound) {
		t.Fatalf("settlement was not blocked for exact IDs %s/%s: request=%+v attempt=%+v reservation=%+v errors=%v/%v/%v usage=%v", requestID, attemptID, request, attempt, reservation, reqErr, attErr, resErr, usageErr)
	}
}

func assertRecoveredAttempt(t *testing.T, dbPath, requestID, attemptID string) {
	t.Helper()
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ledger := sqlite.NewLedger(db)
	request, reqErr := ledger.GetRequest(context.Background(), requestID)
	attempt, attErr := ledger.GetAttempt(context.Background(), attemptID)
	reservation, resErr := ledger.GetReservation(context.Background(), attemptID)
	usage, usageErr := ledger.GetUsage(context.Background(), attemptID)
	if reqErr != nil || attErr != nil || resErr != nil || usageErr != nil || request.State != "interrupted" || attempt.State != "interrupted" || reservation.State != "conservative" || reservation.EffectiveCharge != 10 || usage.AttemptID != attemptID || usage.InputTokens != nil || usage.OutputTokens != nil {
		t.Fatalf("exact interrupted rows not recovered for %s/%s: request=%+v attempt=%+v reservation=%+v usage=%+v errors=%v/%v/%v/%v", requestID, attemptID, request, attempt, reservation, usage, reqErr, attErr, resErr, usageErr)
	}
}

type restartCLIRequest struct {
	ID       string              `json:"id"`
	KeyID    string              `json:"key_id"`
	State    string              `json:"state"`
	Attempts []restartCLIAttempt `json:"attempts"`
}

type restartCLISnapshot struct {
	JSON string
	Rows []restartCLIRequest
}

func readRestartCLIRequests(t *testing.T, binary, dbPath, keyPath, keyID string) restartCLISnapshot {
	t.Helper()
	data, err := runAdminProcess(t, binary, dbPath, keyPath, "usage", "requests", "--key", keyID, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []restartCLIRequest
	if err := json.Unmarshal([]byte(data), &rows); err != nil {
		t.Fatalf("decode CLI request rows: %v", err)
	}
	return restartCLISnapshot{JSON: data, Rows: rows}
}

type restartCLIAttempt struct {
	ID          string  `json:"id"`
	State       string  `json:"state"`
	ErrorReason *string `json:"error_reason"`
	Usage       *struct {
		Input  *int64 `json:"input_tokens"`
		Output *int64 `json:"output_tokens"`
	} `json:"usage"`
	Reservation struct {
		State     string `json:"state"`
		Estimated int64  `json:"estimated_tokens"`
		Charge    int64  `json:"effective_charge"`
	} `json:"reservation"`
}

func assertRestartCLIRequests(t *testing.T, got []restartCLIRequest, keyID, undispatchedID string, dispatched int) {
	t.Helper()
	if len(got) != dispatched+1 {
		t.Fatalf("CLI request count=%d, want %d: %+v", len(got), dispatched+1, got)
	}
	seenRequests, seenAttempts := map[string]bool{}, map[string]bool{}
	var estimateSum, chargeSum int64
	for _, q := range got {
		if q.ID == "" || seenRequests[q.ID] || q.KeyID != keyID || len(q.Attempts) != 1 {
			t.Fatalf("unexpected/duplicate CLI request row: %+v", q)
		}
		seenRequests[q.ID] = true
		a := q.Attempts[0]
		if a.ID == "" || seenAttempts[a.ID] || a.Reservation.Estimated != 10 {
			t.Fatalf("unexpected/duplicate CLI attempt row: %+v", a)
		}
		seenAttempts[a.ID] = true
		estimateSum += a.Reservation.Estimated
		chargeSum += a.Reservation.Charge
		switch q.ID {
		case undispatchedID:
			if q.State != "failed" || a.State != "failed" || a.ErrorReason == nil || *a.ErrorReason != "not_dispatched" || a.Reservation.State != "released" || a.Reservation.Charge != 0 || a.Usage != nil {
				t.Fatalf("undispatched CLI row = %+v", q)
			}
		default:
			if q.State != "interrupted" || a.State != "interrupted" || a.Reservation.State != "conservative" || a.Reservation.Charge != 10 || a.Usage == nil || a.Usage.Input != nil || a.Usage.Output != nil {
				t.Fatalf("dispatched CLI row = %+v", q)
			}
		}
	}
	if len(seenRequests) != dispatched+1 || len(seenAttempts) != dispatched+1 || estimateSum != int64((dispatched+1)*10) || chargeSum != int64(dispatched*10) {
		t.Fatalf("CLI ID/count/charge totals requests=%d attempts=%d estimate=%d charge=%d", len(seenRequests), len(seenAttempts), estimateSum, chargeSum)
	}
}

func assertUndispatchedRecovered(t *testing.T, dbPath, requestID, attemptID string) {
	t.Helper()
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ledger := sqlite.NewLedger(db)
	request, reqErr := ledger.GetRequest(context.Background(), requestID)
	attempt, attemptErr := ledger.GetAttempt(context.Background(), attemptID)
	reservation, reserveErr := ledger.GetReservation(context.Background(), attemptID)
	_, usageErr := ledger.GetUsage(context.Background(), attemptID)
	if reqErr != nil || attemptErr != nil || reserveErr != nil || request.State != "failed" || attempt.State != "failed" || attempt.ErrorReason == nil || *attempt.ErrorReason != "not_dispatched" || reservation.State != "released" || reservation.EffectiveCharge != 0 || !errors.Is(usageErr, sqlite.ErrLedgerNotFound) {
		t.Fatalf("exact pre-intent recovery rows request=%+v attempt=%+v reservation=%+v errors=%v/%v/%v usage=%v", request, attempt, reservation, reqErr, attemptErr, reserveErr, usageErr)
	}
}

func startGatewayProcess(t *testing.T, binary, configPath string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(binary, "-config-format", "yaml", "-config", configPath)
	cmd.Env = append(os.Environ(), "PROTECTED_TEST_CREDENTIAL=synthetic")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd
}

func waitProcessReady(t *testing.T, address string, cmd *exec.Cmd) {
	t.Helper()
	transport := &http.Transport{DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: 100 * time.Millisecond, Transport: transport}
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://" + address + "/readyz")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("gateway did not become ready")
}

func postRestartRequest(address, secret string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, "http://"+address+"/v1/responses", strings.NewReader(`{"model":"model-a","input":"after restart"}`))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	return http.DefaultClient.Do(req)
}

func sendRestartRequest(address, secret string) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := postRestartRequest(address, secret)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}()
	return done
}

func cancelProcess(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if cmd.Process != nil {
		_ = cmd.Process.Signal(os.Interrupt)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("gateway shutdown: %v", err)
	}
}

type preIntentProcess struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
}

func startPreIntentCrashProcess(t *testing.T, configPath string) preIntentProcess {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRestartAccountingCrashHelper$")
	cmd.Env = append(os.Environ(), "PESTIROUTE_M3_032_CRASH_HELPER=1", "PESTIROUTE_M3_032_CONFIG="+configPath, "PROTECTED_TEST_CREDENTIAL=synthetic")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return preIntentProcess{cmd: cmd, stdout: stdout}
}

// TestRestartAccountingCrashHelper is selected only in a child test process.
// The process blocks in the test-only store wrapper after SQLite Admit commits.
func TestRestartAccountingCrashHelper(t *testing.T) {
	if os.Getenv("PESTIROUTE_M3_032_CRASH_HELPER") != "1" {
		return
	}
	configPath := os.Getenv("PESTIROUTE_M3_032_CONFIG")
	c, _, err := loadConfig([]string{"-config-format", "yaml", "-config", configPath})
	if err != nil {
		t.Fatal(err)
	}
	c, err = prepareProtectedConfig(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	c.accounting = postAdmitCrashGate{store: sqliteAccountingStore{ledger: sqlite.NewLedger(c.runtimeDB)}}
	var ready, draining atomic.Bool
	h, _, err := composeHandler(c, &ready, &draining, nil)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", c.Listen)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	ready.Store(true)
	go func() { _ = server.Serve(listener) }()
	select {}
}

type postAdmitCrashGate struct{ store sqliteAccountingStore }

func (g postAdmitCrashGate) Admit(ctx context.Context, in core.AccountingAdmission) error {
	if err := g.store.Admit(ctx, in); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(os.Stdout, "ADMITTED %s %s\n", in.RequestID, in.AttemptID)
	select {}
}
func (g postAdmitCrashGate) BeginAttempt(ctx context.Context, in core.AccountingAdmission) error {
	return g.store.BeginAttempt(ctx, in)
}

func (g postAdmitCrashGate) RecordDispatchIntent(ctx context.Context, attemptID string, at time.Time) error {
	return g.store.RecordDispatchIntent(ctx, attemptID, at)
}

func (g postAdmitCrashGate) FinalizeAttempt(ctx context.Context, terminal core.AccountingTerminal) error {
	return g.store.FinalizeAttempt(ctx, terminal)
}

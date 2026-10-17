package main

import (
	"bufio"
	"context"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	adapter "github.com/blestafist/pestiroute/internal/adapter/responses"
	"github.com/blestafist/pestiroute/internal/connector/antigravity"
	"github.com/blestafist/pestiroute/internal/core"
	secure "github.com/blestafist/pestiroute/internal/crypto"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

func TestAntigravityProtectedYAMLStreamsAndCancelsWithRealHTTPClients(t *testing.T) {
	t.Setenv("PESTIROUTE_ANTIGRAVITY_REAL_TRANSPORT", "1")
	_, dbPath, keyPath := protectedFixture(t)
	accountID, connectorID, model := "antigravity-account", "antigravity", "gemini-2.5-pro"
	var upstreamCalls atomic.Int32
	releaseTerminal := make(chan struct{})
	cancelObserved := make(chan struct{})
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Synthetic gateway output\"}]}}]}}\n\n")
		w.(http.Flusher).Flush()
		if call == 1 {
			select {
			case <-releaseTerminal:
				_, _ = io.WriteString(w, "data: {\"response\":{\"done\":true}}\n\n")
				w.(http.Flusher).Flush()
			case <-r.Context().Done():
			}
			return
		}
		<-r.Context().Done()
		close(cancelObserved)
	}))
	t.Cleanup(upstream.Close)

	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := sqlite.NewAccounts(db).Create(ctx, sqlite.Account{ID: accountID, Connector: connectorID, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	key, err := secure.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := secure.Seal(key, 1, "test-v1", "credentials", "oauth", accountID, []byte(`{"access_token":"SYNTHETIC_ACCESS_TOKEN"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = sqlite.NewCredentials(db).Create(ctx, sqlite.Credential{ID: "oauth", AccountID: accountID, FormatVersion: sealed.FormatVersion, KeyVersion: sealed.KeyVersion, Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext}); err != nil {
		t.Fatal(err)
	}
	policies := sqlite.NewKeyPolicies(db)
	policy, err := policies.GetLatest(ctx, "policy-id-a")
	if err != nil {
		t.Fatal(err)
	}
	policy, err = policies.Update(ctx, policy.ID, policy.Revision, sqlite.UpdateKeyPolicyParams{Enabled: true, Models: []string{model}, Connectors: []string{connectorID}, RPM: 20, TPM: 100000})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(t.TempDir(), "gateway.yaml")
	yaml := fmt.Sprintf(`version: 1
server: {listen: "127.0.0.1:0", max_request_bytes: 1048576, shutdown_timeout: 1s}
storage: {driver: sqlite, path: %q}
secrets: {master_key_file: %q}
connectors:
  - id: antigravity
    kind: connector
    implementation: pestiroute.antigravity.v1internal
    protocols: [openai.responses.v1]
    settings: {project: synthetic-project, model: %q, account_id: %q, endpoint: %s}
routes:
  - id: antigravity-route
    protocol: openai.responses.v1
    mode: translation
    model: %q
    adapter: pestiroute.responses.native
    policy: standard
    budget: {unknown_estimate: reserve, conservative_tokens: 4096}
    targets: [{connector: antigravity, account: %q}]
policies: {standard: policy-id-a}
`, dbPath, keyPath, model, accountID, upstream.URL, model, accountID)
	for _, invalidYAML := range []string{
		strings.Replace(yaml, "project: synthetic-project, ", "", 1),
		strings.Replace(yaml, "endpoint: "+upstream.URL, "endpoint: http://antigravity.synthetic.invalid", 1),
	} {
		if err := os.WriteFile(configPath, []byte(invalidYAML), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadProtectedYAML(configPath); err == nil {
			t.Fatal("invalid Antigravity YAML accepted")
		}
	}
	if err := os.WriteFile(configPath, []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
	protected, err := loadProtectedYAML(configPath)
	if err != nil {
		t.Fatal(err)
	}
	certBundle := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: upstream.Certificate().Raw})
	certPath := filepath.Join(t.TempDir(), "test-roots.pem")
	if err := os.WriteFile(certPath, certBundle, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSL_CERT_FILE", certPath)
	prepared, err := prepareProtectedConfig(ctx, config{protected: &protected, DatabasePath: dbPath, MasterKeyFile: keyPath})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = prepared.runtimeDB.Close()
		_ = prepared.processLock.Close()
	})

	var connector *antigravity.Connector
	var ready, draining atomic.Bool
	ready.Store(true)
	h, closeComponents, err := composeHandlerWithFactory(prepared, &ready, &draining, nil, func(item topologyComponent) core.Component {
		if item.Kind == core.ComponentAdapter {
			return adapter.NewAdapter()
		}
		connector = antigravity.NewConnector()
		return connector
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeComponents(context.Background()) })
	issued, err := sqlite.NewVirtualKeys(prepared.runtimeDB).Create(ctx, sqlite.CreateVirtualKeyParams{PolicyID: policy.ID, PolicyRevision: policy.Revision})
	if err != nil {
		t.Fatal(err)
	}
	gateway := httptest.NewServer(h)
	t.Cleanup(gateway.Close)
	post := func(ctx context.Context) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, gateway.URL+"/v1/responses", strings.NewReader(`{"model":"gemini-2.5-pro","stream":true,"input":"synthetic prompt"}`))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+issued.Secret)
		req.Header.Set("Content-Type", "application/json")
		return gateway.Client().Do(req)
	}

	response, err := post(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(response.Body)
	first := readGatewayUntil(t, reader, "Synthetic gateway output")
	if strings.Contains(first, "response.completed") {
		t.Fatal("gateway exposed completion before controlled upstream released its terminal frame")
	}
	close(releaseTerminal)
	remaining, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(first+string(remaining), "response.completed") {
		t.Fatalf("gateway status=%d incomplete stream=%q", response.StatusCode, first+string(remaining))
	}

	cancelCtx, cancel := context.WithCancel(context.Background())
	response, err = post(cancelCtx)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	cancelReader := bufio.NewReader(response.Body)
	_ = readGatewayUntil(t, cancelReader, "Synthetic gateway output")
	cancel()
	_ = response.Body.Close()
	select {
	case <-cancelObserved:
	case <-time.After(3 * time.Second):
		t.Fatal("downstream cancellation did not cancel the controlled upstream request")
	}
	if upstreamCalls.Load() != 2 {
		t.Fatalf("upstream calls=%d want 2", upstreamCalls.Load())
	}
}

func readGatewayUntil(t *testing.T, reader *bufio.Reader, marker string) string {
	t.Helper()
	var collected strings.Builder
	deadline := time.After(5 * time.Second)
	for !strings.Contains(collected.String(), marker) {
		select {
		case <-deadline:
			t.Fatalf("stream did not deliver %q; received %q", marker, collected.String())
		default:
		}
		line, err := reader.ReadString('\n')
		collected.WriteString(line)
		if err != nil {
			t.Fatalf("stream ended before %q: %v (%q)", marker, err, collected.String())
		}
	}
	return collected.String()
}

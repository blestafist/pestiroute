package main

import (
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

	adapter "github.com/blestafist/pestiroute/internal/adapter/responses"
	"github.com/blestafist/pestiroute/internal/connector/antigravity"
	"github.com/blestafist/pestiroute/internal/core"
	secure "github.com/blestafist/pestiroute/internal/crypto"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

func TestAntigravityCredentialIDProtectedExecution(t *testing.T) {
	t.Setenv("PESTIROUTE_ANTIGRAVITY_REAL_TRANSPORT", "1")
	var calls atomic.Int32
	var expectedAuthorization string
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if got := r.Header.Get("Authorization"); got != expectedAuthorization {
			t.Errorf("fake upstream authorization did not match selected credential")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"synthetic credential execution\"}]}}]}}\n\ndata: {\"response\":{\"done\":true}}\n\n")
	}))
	t.Cleanup(upstream.Close)
	certPath := filepath.Join(t.TempDir(), "fake-upstream.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: upstream.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSL_CERT_FILE", certPath)

	tests := []struct {
		name, credentialID, credentialRow, credentialAccount, token string
		emptySetting                                                bool
		wantSuccess                                                 bool
		wantCalls                                                   int32
	}{
		{name: "custom row executes", credentialID: "custom-oauth", credentialRow: "custom-oauth", credentialAccount: "antigravity-account", token: "SYNTHETIC_CUSTOM_TOKEN", wantSuccess: true, wantCalls: 1},
		{name: "omitted defaults oauth", credentialRow: "oauth", credentialAccount: "antigravity-account", token: "SYNTHETIC_OAUTH_TOKEN", wantSuccess: true, wantCalls: 1},
		{name: "empty defaults oauth", credentialRow: "oauth", credentialAccount: "antigravity-account", token: "SYNTHETIC_OAUTH_TOKEN", emptySetting: true, wantSuccess: true, wantCalls: 1},
		{name: "missing row fails closed", credentialID: "missing-oauth", credentialRow: "oauth", credentialAccount: "antigravity-account", token: "SYNTHETIC_OAUTH_TOKEN", wantCalls: 0},
		{name: "wrong account does not fallback", credentialID: "custom-oauth", credentialRow: "custom-oauth", credentialAccount: "other-account", token: "SYNTHETIC_CUSTOM_TOKEN", wantCalls: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := calls.Load()
			expectedAuthorization = "Bearer " + tt.token
			_, dbPath, keyPath := protectedFixture(t)
			db, err := sqlite.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			accounts := sqlite.NewAccounts(db)
			for _, account := range []sqlite.Account{{ID: "antigravity-account", Connector: "antigravity", Enabled: true}, {ID: "other-account", Connector: "antigravity", Enabled: true}} {
				if _, err := accounts.Create(ctx, account); err != nil {
					t.Fatal(err)
				}
			}
			key, err := secure.LoadMasterKey(keyPath)
			if err != nil {
				t.Fatal(err)
			}
			addCredential := func(id, accountID, value string) {
				t.Helper()
				sealed, err := secure.Seal(key, 2, "test-v1", "credentials", id, accountID, fmt.Appendf(nil, `{"access_token":%q,"project_id":"synthetic-project"}`, value))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := sqlite.NewCredentials(db).Create(ctx, sqlite.Credential{ID: id, AccountID: accountID, FormatVersion: sealed.FormatVersion, KeyVersion: sealed.KeyVersion, Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext}); err != nil {
					t.Fatal(err)
				}
			}
			if tt.name == "custom row executes" {
				addCredential("oauth", "antigravity-account", "SYNTHETIC_OAUTH_TOKEN")
			}
			if tt.name == "wrong account does not fallback" {
				addCredential("oauth", "antigravity-account", "SYNTHETIC_OAUTH_TOKEN")
			}
			addCredential(tt.credentialRow, tt.credentialAccount, tt.token)
			if _, err := sqlite.NewKeyPolicies(db).Create(ctx, sqlite.CreateKeyPolicyParams{ID: "antigravity-policy", Enabled: true, Models: []string{"gemini-2.5-pro"}, Connectors: []string{"antigravity"}, RPM: 10, TPM: 100000}); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}

			configPath := filepath.Join(t.TempDir(), "gateway.yaml")
			credentialSetting := ""
			if tt.credentialID != "" {
				credentialSetting = fmt.Sprintf(", credential_id: %q", tt.credentialID)
			} else if tt.emptySetting {
				credentialSetting = `, credential_id: ""`
			}
			body := fmt.Sprintf(`version: 1
server: {listen: "127.0.0.1:0", max_request_bytes: 1048576, shutdown_timeout: 1s}
storage: {driver: sqlite, path: %q}
secrets: {master_key_file: %q}
connectors:
  - id: antigravity
    kind: connector
    implementation: pestiroute.antigravity.v1internal
    protocols: [openai.responses.v1]
    settings: {project: synthetic-project, model: gemini-2.5-pro, account_id: antigravity-account%s, endpoint: %s}
routes:
  - id: translated
    protocol: openai.responses.v1
    mode: translation
    model: gemini-2.5-pro
    adapter: pestiroute.responses.native
    policy: standard
    budget: {unknown_estimate: reserve, conservative_tokens: 4096}
    targets: [{connector: antigravity, account: antigravity-account}]
policies: {standard: antigravity-policy}
`, dbPath, keyPath, credentialSetting, upstream.URL)
			if err := os.WriteFile(configPath, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			protected, err := loadProtectedYAML(configPath)
			if err != nil {
				t.Fatalf("load protected config: %v", err)
			}
			if tt.name == "custom row executes" {
				invalid := protected
				invalid.Connectors = append([]protectedConnector(nil), protected.Connectors...)
				invalid.Connectors[0].Settings.CredentialID = " \t"
				if _, err := prepareProtectedConfig(ctx, config{protected: &invalid, DatabasePath: dbPath, MasterKeyFile: keyPath}); err == nil || !strings.Contains(err.Error(), "credential_id cannot be whitespace") {
					t.Fatalf("protected startup accepted whitespace credential_id: %v", err)
				}
			}
			prepared, err := prepareProtectedConfig(ctx, config{protected: &protected, DatabasePath: dbPath, MasterKeyFile: keyPath})
			if err != nil {
				t.Fatal(err)
			}
			defer prepared.runtimeDB.Close()
			defer prepared.processLock.Close()
			if want := map[bool]string{true: tt.credentialID, false: "oauth"}[tt.credentialID != ""]; prepared.Components[0].CredentialEnv != want {
				t.Fatalf("credential projection=%q, want %q", prepared.Components[0].CredentialEnv, want)
			}

			var ready, draining atomic.Bool
			ready.Store(true)
			h, closeComponents, err := composeHandlerWithFactory(prepared, &ready, &draining, nil, func(item topologyComponent) core.Component {
				if item.Kind == core.ComponentAdapter {
					return adapter.NewAdapter()
				}
				return antigravity.NewConnector()
			})
			if err != nil {
				t.Fatal(err)
			}
			defer closeComponents(context.Background())
			policy, err := sqlite.NewKeyPolicies(prepared.runtimeDB).GetLatest(ctx, "antigravity-policy")
			if err != nil {
				t.Fatal(err)
			}
			issued, err := sqlite.NewVirtualKeys(prepared.runtimeDB).Create(ctx, sqlite.CreateVirtualKeyParams{PolicyID: policy.ID, PolicyRevision: policy.Revision})
			if err != nil {
				t.Fatal(err)
			}
			gateway := httptest.NewServer(h)
			defer gateway.Close()
			req, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/responses", strings.NewReader(`{"model":"gemini-2.5-pro","stream":true,"input":"synthetic"}`))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+issued.Secret)
			req.Header.Set("Content-Type", "application/json")
			resp, err := gateway.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			response, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantSuccess && (resp.StatusCode != http.StatusOK || !strings.Contains(string(response), "synthetic credential execution")) {
				t.Fatalf("execution status=%d calls=%d body=%q", resp.StatusCode, calls.Load()-before, response)
			}
			if !tt.wantSuccess && resp.StatusCode == http.StatusOK {
				t.Fatalf("invalid credential unexpectedly executed: %q", response)
			}
			if got := calls.Load() - before; got != tt.wantCalls {
				t.Fatalf("fake upstream calls=%d, want %d", got, tt.wantCalls)
			}
		})
	}

	t.Run("whitespace rejected", func(t *testing.T) {
		base := nativeSettings{Project: "synthetic-project", Model: "gemini-2.5-pro", AccountID: "antigravity-account", CredentialID: " \t"}
		if err := validateAntigravitySettings("settings", base); err == nil || !strings.Contains(err.Error(), "credential_id cannot be whitespace") {
			t.Fatalf("whitespace credential_id accepted or unclear: %v", err)
		}
	})
}

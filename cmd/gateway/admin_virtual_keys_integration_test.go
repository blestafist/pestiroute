package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	adminapi "github.com/blestafist/pestiroute/internal/admin"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

func TestAdminVirtualKeyHTTPAndInferenceListeners(t *testing.T) {
	t.Setenv("PROTECTED_TEST_CREDENTIAL", "synthetic")
	t.Setenv("PESTIROUTE_ADMIN_TEST_TOKEN", "operator-token")
	_, dbPath, keyPath := protectedFixture(t)
	ctx := context.Background()
	adminDB, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	adminHandler, err := adminapi.New(adminapi.Config{Address: "127.0.0.1:9094", TokenEnv: "PESTIROUTE_ADMIN_TEST_TOKEN", DB: adminDB, ReadTimeout: time.Second, WriteTimeout: time.Second, IdleTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	adminServer := httptest.NewServer(adminHandler)
	t.Cleanup(func() { adminServer.Close(); _ = adminDB.Close() })
	adminCall := func(method, path, body string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, adminServer.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "127.0.0.1:9094"
		req.Header.Set("Authorization", "Bearer operator-token")
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := adminServer.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	createPolicy := func(id string, models, connectors []string, rpm, tpm int) int64 {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"id": id, "models": models, "connectors": connectors, "rpm": rpm, "tpm": tpm})
		resp := adminCall(http.MethodPost, "/admin/v1/policies", string(body))
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create policy %s: status %d", id, resp.StatusCode)
		}
		var policy struct {
			Revision int64 `json:"revision"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&policy); err != nil {
			t.Fatal(err)
		}
		return policy.Revision
	}
	const allowedPolicy = "m11-http-allowed"
	createPolicy(allowedPolicy, []string{"blocked-model"}, []string{"upstream"}, 10, 1000)

	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"completed","usage":{"input_tokens":3,"output_tokens":2}}`))
	}))
	defer upstream.Close()
	c := fixtureProtectedConfig(dbPath, keyPath, "127.0.0.1:0")
	c.protected.Connectors[0].Settings.BaseURL = upstream.URL + "/v1/responses"
	c.protected.Policies["standard"] = allowedPolicy
	c.protected.Routes[0].Model = "blocked-model"
	prepared, err := prepareProtectedConfig(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	var ready, draining atomic.Bool
	ready.Store(true)
	h, closeComponents, err := composeHandler(prepared, &ready, &draining, nil)
	if err != nil {
		_ = prepared.runtimeDB.Close()
		_ = prepared.processLock.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeComponents(context.Background()) })
	inferenceServer := httptest.NewServer(h)
	t.Cleanup(inferenceServer.Close)
	createKey := func(policyID string, revision int) (string, string) {
		t.Helper()
		resp := adminCall(http.MethodPost, "/admin/v1/keys", `{"policy_id":"`+policyID+`","policy_revision":`+strconv.Itoa(revision)+`}`)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create key %s: status %d", policyID, resp.StatusCode)
		}
		var issued struct {
			Secret string `json:"secret"`
			Key    struct {
				ID string `json:"id"`
			} `json:"key"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&issued); err != nil {
			t.Fatal(err)
		}
		return issued.Secret, issued.Key.ID
	}
	postInferenceResult := func(secret, model, input string) (int, []byte) {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"model": model, "input": input})
		req, err := http.NewRequest(http.MethodPost, inferenceServer.URL+"/v1/responses", strings.NewReader(string(body)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+secret)
		req.Header.Set("Content-Type", "application/json")
		resp, err := inferenceServer.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		result, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, result
	}
	postInference := func(secret, model, input string) int {
		status, _ := postInferenceResult(secret, model, input)
		return status
	}
	secret, keyID := createKey(allowedPolicy, 1)
	if got := postInference(secret, "blocked-model", "first"); got != http.StatusOK {
		t.Fatalf("new API key inference=%d", got)
	}
	update := adminCall(http.MethodPut, "/admin/v1/policies/"+allowedPolicy, `{"expected_revision":1,"models":[]}`)
	if update.StatusCode != http.StatusOK {
		update.Body.Close()
		t.Fatalf("restrict model policy status=%d", update.StatusCode)
	}
	update.Body.Close()
	restrictedSecret, _ := createKey(allowedPolicy, 2)
	callsBeforeDeniedModel := upstreamCalls.Load()
	if got, body := postInferenceResult(restrictedSecret, "blocked-model", "model denial"); got != http.StatusForbidden || !strings.Contains(string(body), `"code":"permission_denied"`) || upstreamCalls.Load() != callsBeforeDeniedModel {
		t.Fatalf("model policy denial status=%d body=%s upstream calls=%d want unchanged=%d", got, body, upstreamCalls.Load(), callsBeforeDeniedModel)
	}
	createPolicy("m11-http-low-rpm", []string{"blocked-model"}, []string{"upstream"}, 1, 1000)
	rpmSecret, _ := createKey("m11-http-low-rpm", 1)
	if got := postInference(rpmSecret, "blocked-model", "first within rpm"); got != http.StatusOK {
		t.Fatalf("initial RPM request=%d", got)
	}
	if got := postInference(rpmSecret, "blocked-model", "rpm denial"); got != http.StatusTooManyRequests {
		t.Fatalf("RPM limit status=%d", got)
	}
	if got := adminCall(http.MethodDelete, "/admin/v1/keys/"+keyID, ""); got.StatusCode != http.StatusOK {
		got.Body.Close()
		t.Fatalf("revoke key status=%d", got.StatusCode)
	} else {
		got.Body.Close()
	}
	if got := postInference(secret, "blocked-model", "revoked"); got != http.StatusUnauthorized {
		t.Fatalf("revoked key admission=%d", got)
	}

	createPolicy("m11-http-bad-connector", []string{"blocked-model"}, []string{"not-the-route"}, 10, 1000)
	badConnectorSecret, _ := createKey("m11-http-bad-connector", 1)
	callsBeforeBadConnector := upstreamCalls.Load()
	if got := postInference(badConnectorSecret, "blocked-model", "connector denial"); got != http.StatusForbidden || upstreamCalls.Load() != callsBeforeBadConnector {
		t.Fatalf("connector allowlist status=%d upstream calls=%d", got, upstreamCalls.Load())
	}
	createPolicy("m11-http-low-tpm", []string{"blocked-model"}, []string{"upstream"}, 10, 1)
	lowTPMSecret, _ := createKey("m11-http-low-tpm", 1)
	if got := postInference(lowTPMSecret, "blocked-model", "token budget"); got != http.StatusTooManyRequests {
		t.Fatalf("TPM limit status=%d", got)
	}
}

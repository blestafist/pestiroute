package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

func TestPolicyAndVirtualKeyAPI(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "admin.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PESTIROUTE_ADMIN_TEST_TOKEN", "operator-secret")
	h, err := New(Config{Address: "127.0.0.1:9093", TokenEnv: "PESTIROUTE_ADMIN_TEST_TOKEN", DB: db, ReadTimeout: time.Second, WriteTimeout: time.Second, IdleTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Host = "127.0.0.1:9093"
		r.Header.Set("Authorization", "Bearer operator-secret")
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if got := call("POST", "/admin/v1/policies", `{"id":"p","models":["m"],"connectors":["c"],"rpm":10,"tpm":100}`).Code; got != 201 {
		t.Fatalf("create policy=%d", got)
	}
	if got := call("POST", "/admin/v1/policies", `{"id":"extra","models":[],"connectors":[],"rpm":0,"tpm":0,"x":1}`).Code; got != 400 {
		t.Fatalf("extra field=%d", got)
	}
	for _, tc := range []struct{ id, body string }{
		{"missing-rpm", `{"id":"missing-rpm","models":[],"connectors":[],"tpm":0}`},
		{"null-rpm", `{"id":"null-rpm","models":[],"connectors":[],"rpm":null,"tpm":0}`},
		{"missing-tpm", `{"id":"missing-tpm","models":[],"connectors":[],"rpm":0}`},
		{"null-tpm", `{"id":"null-tpm","models":[],"connectors":[],"rpm":0,"tpm":null}`},
	} {
		if got := call("POST", "/admin/v1/policies", tc.body).Code; got != 400 {
			t.Fatalf("%s status=%d", tc.id, got)
		}
		if _, err := sqlite.NewKeyPolicies(db).GetLatest(ctx, tc.id); !errors.Is(err, sqlite.ErrKeyPolicyNotFound) {
			t.Fatalf("%s persisted: %v", tc.id, err)
		}
	}
	if got := call("GET", "/admin/v1/policies?limit=-1", "").Code; got != 400 {
		t.Fatalf("invalid limit=%d", got)
	}
	if got := call("PUT", "/admin/v1/policies/p", `{"expected_revision":1,"rpm":20}`).Code; got != 200 {
		t.Fatalf("update policy=%d", got)
	}
	if got := call("GET", "/admin/v1/policies/p?revision=1", ""); got.Code != 200 || !strings.Contains(got.Body.String(), `"rpm":10`) {
		t.Fatalf("immutable policy revision: %d %s", got.Code, got.Body.String())
	}
	if got := call("GET", "/admin/v1/policies?enabled=true&limit=1&offset=0", ""); got.Code != 200 || !strings.Contains(got.Body.String(), `"total":1`) {
		t.Fatalf("filtered policy page: %d %s", got.Code, got.Body.String())
	}
	if got := call("PUT", "/admin/v1/policies/p", `{"expected_revision":1,"rpm":30}`).Code; got != 409 {
		t.Fatalf("stale policy CAS=%d", got)
	}
	if got := call("PUT", "/admin/v1/policies/p", `{"expected_revision":2,"enabled":true}`).Code; got != 400 {
		t.Fatalf("enabled update accepted=%d", got)
	}
	if got := call("PUT", "/admin/v1/policies/p", `{"expected_revision":2,"models":null}`).Code; got != 400 {
		t.Fatalf("null optional policy field accepted=%d", got)
	}
	created := call("POST", "/admin/v1/keys", `{"policy_id":"p","policy_revision":2}`)
	if created.Code != 201 || !strings.Contains(created.Body.String(), `"secret":"prv_`) {
		t.Fatalf("create key=%d %s", created.Code, created.Body.String())
	}
	var issued struct {
		Secret string `json:"secret"`
		Key    struct {
			ID    string `json:"id"`
			KeyID string `json:"key_id"`
		} `json:"key"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &issued); err != nil {
		t.Fatal(err)
	}
	secret := issued.Secret
	if strings.Contains(created.Body.String(), `"digest"`) {
		t.Fatal("digest leaked on creation")
	}
	id, publicID := issued.Key.ID, issued.Key.KeyID
	if principal, err := sqlite.NewVirtualKeys(db).Verify(ctx, secret); err != nil || principal.PolicyRevision != 2 {
		t.Fatalf("created key not immediately usable: %+v %v", principal, err)
	}
	for _, path := range []string{"/admin/v1/keys?limit=1", "/admin/v1/keys/" + id, "/admin/v1/keys/" + publicID} {
		got := call("GET", path, "")
		if got.Code != 200 || strings.Contains(got.Body.String(), secret) || strings.Contains(got.Body.String(), `"digest"`) {
			t.Fatalf("redacted %s: %d %s", path, got.Code, got.Body.String())
		}
	}
	if got := call("PUT", "/admin/v1/keys/"+id+"/policy", `{"policy_id":"p","policy_revision":1,"expected_revision":1}`); got.Code != 200 || strings.Contains(got.Body.String(), secret) {
		t.Fatalf("reassign=%d %s", got.Code, got.Body.String())
	}
	if got := call("PUT", "/admin/v1/keys/"+id+"/policy", `{"policy_id":"p","policy_revision":1,"expected_revision":1}`).Code; got != 409 {
		t.Fatalf("stale key CAS=%d", got)
	}
	var keyWG sync.WaitGroup
	keyResults := make(chan int, 2)
	for _, revision := range []int{1, 2} {
		keyWG.Add(1)
		go func(revision int) {
			defer keyWG.Done()
			keyResults <- call("PUT", "/admin/v1/keys/"+id+"/policy", `{"policy_id":"p","policy_revision":`+strconv.Itoa(revision)+`,"expected_revision":2}`).Code
		}(revision)
	}
	keyWG.Wait()
	close(keyResults)
	keyCounts := map[int]int{}
	for status := range keyResults {
		keyCounts[status]++
	}
	if keyCounts[200] != 1 || keyCounts[409] != 1 {
		t.Fatalf("key CAS race=%v", keyCounts)
	}
	var wg sync.WaitGroup
	results := make(chan int, 2)
	for _, rpm := range []int{30, 40} {
		wg.Add(1)
		go func(rpm int) {
			defer wg.Done()
			results <- call("PUT", "/admin/v1/policies/p", `{"expected_revision":2,"rpm":`+strconv.Itoa(rpm)+`}`).Code
		}(rpm)
	}
	wg.Wait()
	close(results)
	counts := map[int]int{}
	for status := range results {
		counts[status]++
	}
	if counts[200] != 1 || counts[409] != 1 {
		t.Fatalf("policy CAS race=%v", counts)
	}
	for range 2 {
		revoked := call(http.MethodDelete, "/admin/v1/keys/"+id, "")
		if revoked.Code != 200 || !strings.Contains(revoked.Body.String(), `"revoked":true`) || strings.Contains(revoked.Body.String(), secret) {
			t.Fatalf("revoke=%d %s", revoked.Code, revoked.Body.String())
		}
	}
	if _, err := sqlite.NewVirtualKeys(db).Verify(ctx, secret); err == nil {
		t.Fatal("revoked key still authenticates")
	}
	if got := call("POST", "/admin/v1/keys", `{"policy_id":"missing","policy_revision":1}`).Code; got != 404 {
		t.Fatalf("missing policy=%d", got)
	}
	if got := call("PUT", "/admin/v1/policies/p", `{"expected_revision":-1,"rpm":-1}`).Code; got != 400 {
		t.Fatalf("invalid update=%d", got)
	}
}

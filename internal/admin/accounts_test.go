package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	secure "github.com/blestafist/pestiroute/internal/crypto"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

func TestAccountAndWriteOnlyCredentialAPI(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "admin.db")
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	accounts := sqlite.NewAccounts(db)
	for _, a := range []sqlite.Account{{ID: "a", Connector: "c", Enabled: true}, {ID: "b", Connector: "c", Enabled: false}, {ID: "z", Connector: "other", Enabled: true}} {
		if _, err := accounts.Create(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	keyPath := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(keyPath, bytes.Repeat([]byte{0x5a}, secure.KeySize), 0600); err != nil {
		t.Fatal(err)
	}
	key, err := secure.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	sessions := sqlite.NewAuthSessions(db)
	now := time.Now().UTC()
	if _, err := sessions.CreateInteractiveSession(ctx, "interactive", "a", "c", 1, now.Add(time.Hour), []byte("sensitive-session-state"), key, "v1", now); err != nil {
		t.Fatal(err)
	}
	if err := sessions.CreateRefreshMarker(ctx, "refresh", "a", "c", 1, now); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PESTIROUTE_ADMIN_TEST_TOKEN", "operator-secret")
	h, err := New(Config{Address: "127.0.0.1:9093", TokenEnv: "PESTIROUTE_ADMIN_TEST_TOKEN", DB: db, MasterKey: key, ReadTimeout: time.Second, WriteTimeout: time.Second, IdleTimeout: time.Second})
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
	callNoContentType := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Host = "127.0.0.1:9093"
		r.Header.Set("Authorization", "Bearer operator-secret")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if got := call(http.MethodPost, "/admin/v1/accounts", `{"id":"a","connector":"c"}`).Code; got != 409 {
		t.Fatalf("duplicate account=%d", got)
	}
	if got := call(http.MethodPost, "/admin/v1/accounts", `{"id":"null-enabled","connector":"c","enabled":null}`).Code; got != 400 {
		t.Fatalf("null enabled accepted=%d", got)
	}
	if got := call(http.MethodGet, "/admin/v1/accounts?limit=501", "").Code; got != 400 {
		t.Fatalf("bad pagination=%d", got)
	}
	if got := call(http.MethodGet, "/admin/v1/accounts?enabled=TRUE", "").Code; got != 400 {
		t.Fatalf("bad filter=%d", got)
	}
	if got := call(http.MethodGet, "/admin/v1/accounts?connector=c&enabled=true&limit=1&offset=9", ""); got.Code != 200 || !strings.Contains(got.Body.String(), `"offset":9`) || !strings.Contains(got.Body.String(), `"total":1`) {
		t.Fatalf("filtered/clamped page: %d %s", got.Code, got.Body.String())
	}
	if got := call(http.MethodGet, "/admin/v1/accounts?connector=c&enabled=false", ""); got.Code != 200 || !strings.Contains(got.Body.String(), `"id":"b"`) || strings.Contains(got.Body.String(), `"id":"a"`) {
		t.Fatalf("filtered list: %d %s", got.Code, got.Body.String())
	}
	if got := call(http.MethodPost, "/admin/v1/accounts", `{"ID":"case-alias","connector":"c"}`).Code; got != 400 {
		t.Fatalf("case-insensitive schema key accepted: %d", got)
	}
	if got := callNoContentType(http.MethodPost, "/admin/v1/accounts", `{"id":"x","connector":"c"}`).Code; got != 415 {
		t.Fatalf("missing content type=%d", got)
	}
	if got := call(http.MethodGet, "/admin/v1/accounts/missing", "").Code; got != 404 {
		t.Fatalf("missing account=%d", got)
	}
	const secret = "credential-secret-marker"
	created := call(http.MethodPost, "/admin/v1/accounts/a/credentials", `{"id":"cred","secret":"`+secret+`"}`)
	if created.Code != 201 || strings.Contains(created.Body.String(), secret) {
		t.Fatalf("credential create status=%d body=%s", created.Code, created.Body.String())
	}
	if got := call(http.MethodPost, "/admin/v1/accounts/a/credentials", `{"id":"cred","secret":"duplicate-secret"}`).Code; got != 409 {
		t.Fatalf("duplicate credential=%d", got)
	}
	if got := call(http.MethodPost, "/admin/v1/accounts/missing/credentials", `{"id":"cred","secret":"missing-secret"}`).Code; got != 404 {
		t.Fatalf("credential missing account=%d", got)
	}
	stored, err := sqlite.NewCredentials(db).Get(ctx, "a", "cred")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored.Ciphertext), secret) {
		t.Fatal("credential plaintext stored")
	}
	plain, err := sqlite.NewCredentials(db).GetDecrypted(ctx, "a", "cred", key)
	if err != nil || string(plain) != secret {
		t.Fatalf("decrypt stored secret: %v", err)
	}
	clear(plain)
	large := strings.Repeat("x", 64<<10)
	if got := call(http.MethodPost, "/admin/v1/accounts/a/credentials", `{"id":"large","secret":"`+large+`"}`).Code; got != 201 {
		t.Fatalf("64KiB credential secret=%d", got)
	}
	escaped := strings.Repeat(`\u0000`, 64<<10)
	if got := call(http.MethodPost, "/admin/v1/accounts/a/credentials", `{"id":"escaped","secret":"`+escaped+`"}`).Code; got != 201 {
		t.Fatalf("escaped 64KiB credential secret=%d", got)
	}
	updated := call(http.MethodPut, "/admin/v1/accounts/a/credentials/cred", `{"secret":"replacement-secret-marker","expected_revision":1}`)
	if updated.Code != 200 || strings.Contains(updated.Body.String(), "replacement-secret-marker") || !strings.Contains(updated.Body.String(), `"revision":2`) {
		t.Fatalf("replace: %d %s", updated.Code, updated.Body.String())
	}
	if got := call(http.MethodPut, "/admin/v1/accounts/a/credentials/cred", `{"secret":"another-secret","expected_revision":1}`).Code; got != 409 {
		t.Fatalf("stale revision=%d", got)
	}
	if got := call(http.MethodPut, "/admin/v1/accounts/a/credentials/missing", `{"secret":"missing-secret","expected_revision":1}`).Code; got != 404 {
		t.Fatalf("missing credential=%d", got)
	}
	var wg sync.WaitGroup
	results := make(chan int, 2)
	for _, value := range []string{"cas-winner-a", "cas-winner-b"} {
		wg.Add(1)
		go func(secret string) {
			defer wg.Done()
			results <- call(http.MethodPut, "/admin/v1/accounts/a/credentials/cred", `{"secret":"`+secret+`","expected_revision":2}`).Code
		}(value)
	}
	wg.Wait()
	close(results)
	statusCounts := map[int]int{}
	for status := range results {
		statusCounts[status]++
	}
	if statusCounts[200] != 1 || statusCounts[409] != 1 {
		t.Fatalf("CAS race statuses: %v", statusCounts)
	}
	if got := call(http.MethodPut, "/admin/v1/accounts/a/enabled", `{"enabled":false}`).Code; got != 200 {
		t.Fatalf("disable=%d", got)
	}
	var interactive, refresh, quarantine string
	if err := db.QueryRow(`SELECT lifecycle FROM auth_sessions WHERE id='interactive'`).Scan(&interactive); err != nil || interactive != "consumed" {
		t.Fatalf("interactive session lifecycle=%q err=%v", interactive, err)
	}
	if err := db.QueryRow(`SELECT lifecycle,quarantine_reason FROM auth_sessions WHERE id='refresh'`).Scan(&refresh, &quarantine); err != nil || refresh != "uncertain" || quarantine != "ambiguous_result" {
		t.Fatalf("refresh state=%q reason=%q err=%v", refresh, quarantine, err)
	}
	var dto map[string]any
	if err := json.Unmarshal(call(http.MethodGet, "/admin/v1/accounts/a", "").Body.Bytes(), &dto); err != nil || dto["enabled"] != false {
		t.Fatalf("account get: %v %v", dto, err)
	}
	for _, path := range []string{"/admin/v1/status", "/admin/v1/accounts/a"} {
		if body := call(http.MethodGet, path, "").Body.String(); strings.Contains(body, secret) || strings.Contains(body, "replacement-secret-marker") {
			t.Fatalf("secret leaked from %s: %s", path, body)
		}
	}
	if response := call(http.MethodGet, "/admin/v1/accounts/a/credentials/cred", ""); response.Code != 404 || strings.Contains(response.Body.String(), secret) {
		t.Fatalf("credential read route exposed data: %d %s", response.Code, response.Body.String())
	}
	if body := call(http.MethodPost, "/admin/v1/accounts/a/credentials", `{"id":"cred","secret":"error-secret-marker"}`).Body.String(); strings.Contains(body, "error-secret-marker") {
		t.Fatalf("secret leaked in duplicate error: %s", body)
	}
	if body := call(http.MethodPost, "/admin/v1/accounts", `{"id":"x","connector":"c","extra":true}`).Body.String(); !strings.Contains(body, `"code":"invalid_request"`) {
		t.Fatalf("unknown field accepted: %s", body)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	restartedKey, err := secure.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	plain, err = sqlite.NewCredentials(db).GetDecrypted(ctx, "a", "cred", restartedKey)
	if err != nil || string(plain) != "cas-winner-a" && string(plain) != "cas-winner-b" {
		t.Fatalf("restart credential read: %v", err)
	}
	clear(plain)
}

func TestCredentialSecretDecoderOwnedBufferIsClearable(t *testing.T) {
	input := []byte(`{"id":"id","secret":"a\u0062\n\uD83D\uDE00"}`)
	parsed, ok := parseCredentialBody(input, false)
	if !ok || string(parsed.secret) != "ab\n😀" {
		t.Fatalf("secret decode=%q ok=%t", parsed.secret, ok)
	}
	clear(parsed.secret)
	if !bytes.Equal(parsed.secret, make([]byte, len(parsed.secret))) {
		t.Fatal("owned secret buffer was not zeroed")
	}
}

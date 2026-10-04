package codex

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func loadAuthFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "auth", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func parseAuthFixtureHashes(manifest []byte) (map[string]string, error) {
	hashes := make(map[string]string)
	lines := strings.Split(string(manifest), "\n")
	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "| File | SHA-256 |" {
			start = i
			break
		}
	}
	if start < 0 || start+1 >= len(lines) || strings.TrimSpace(lines[start+1]) != "| --- | --- |" {
		return nil, fmt.Errorf("missing or malformed SHA-256 table header")
	}
	for i, line := range lines[start+2:] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !strings.HasPrefix(strings.TrimSpace(line), "|") {
			return nil, fmt.Errorf("unexpected content in SHA-256 table at line %d", start+i+3)
		}
		parts := strings.Split(line, "|")
		if len(parts) != 4 {
			return nil, fmt.Errorf("malformed SHA-256 row at line %d", start+i+3)
		}
		nameCell, hashCell := strings.TrimSpace(parts[1]), strings.TrimSpace(parts[2])
		name, sum := strings.Trim(nameCell, "`"), strings.Trim(hashCell, "`")
		if nameCell != "`"+name+"`" || hashCell != "`"+sum+"`" || !strings.HasSuffix(name, ".json") || name == ".json" || strings.ContainsAny(name, `/\\`) {
			return nil, fmt.Errorf("invalid fixture name/hash cell at line %d", start+i+3)
		}
		if len(sum) != sha256.Size*2 {
			return nil, fmt.Errorf("invalid SHA-256 length for %s", name)
		}
		if _, err := hex.DecodeString(sum); err != nil {
			return nil, fmt.Errorf("invalid SHA-256 for %s: %w", name, err)
		}
		if _, duplicate := hashes[name]; duplicate {
			return nil, fmt.Errorf("duplicate SHA-256 row for %s", name)
		}
		hashes[name] = sum
	}
	if len(hashes) == 0 {
		return nil, fmt.Errorf("SHA-256 table contains no fixture rows")
	}
	return hashes, nil
}

func matchesSHA256(data []byte, want string) bool {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]) == want
}

func decodeAuthFixture(t *testing.T, name string) map[string]json.RawMessage {
	t.Helper()
	var value map[string]json.RawMessage
	if err := json.Unmarshal(loadAuthFixture(t, name), &value); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	return value
}

func authString(t *testing.T, fields map[string]json.RawMessage, name string) string {
	t.Helper()
	var value string
	if err := json.Unmarshal(fields[name], &value); err != nil || value == "" {
		t.Fatalf("field %q = %q, err=%v", name, fields[name], err)
	}
	return value
}

func authInteger(t *testing.T, fields map[string]json.RawMessage, name string) int64 {
	t.Helper()
	var value int64
	if err := json.Unmarshal(fields[name], &value); err != nil {
		t.Fatalf("field %q = %q, err=%v", name, fields[name], err)
	}
	return value
}

func TestAuthFixtureIntegrityAndMalformedInput(t *testing.T) {
	manifest, err := os.ReadFile(filepath.Join("testdata", "auth", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	hashes, err := parseAuthFixtureHashes(manifest)
	if err != nil {
		t.Fatal(err)
	}
	diskJSON := make(map[string]bool)
	entries, err := os.ReadDir(filepath.Join("testdata", "auth"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") {
			diskJSON[entry.Name()] = true
		}
	}
	for name := range diskJSON {
		if _, ok := hashes[name]; !ok {
			t.Errorf("fixture %s is missing from manifest", name)
		}
	}
	for name := range hashes {
		if !diskJSON[name] {
			t.Errorf("manifest fixture %s is missing from testdata/auth", name)
		}
	}
	for name, want := range hashes {
		if len(want) != 64 || !matchesSHA256(loadAuthFixture(t, name), want) {
			t.Errorf("SHA-256 mismatch for %s", name)
		}
	}
	original := loadAuthFixture(t, "start-success.json")
	mutated := append([]byte(nil), original...)
	mutated[0] ^= 1
	if matchesSHA256(mutated, hashes["start-success.json"]) {
		t.Fatal("hash mismatch was not detected for mutated fixture copy")
	}
	var malformed any
	if err := json.Unmarshal(loadAuthFixture(t, "malformed.json"), &malformed); err == nil {
		t.Fatal("malformed fixture unexpectedly decoded")
	}

	for _, name := range []string{"start-success.json", "start-string-interval.json"} {
		fields := decodeAuthFixture(t, name)
		for _, key := range []string{"device_code", "user_code", "verification_uri"} {
			authString(t, fields, key)
		}
		if authInteger(t, fields, "expires_in") != 900 {
			t.Fatalf("%s expires_in mismatch", name)
		}
		interval := fields["interval"]
		if name == "start-success.json" && string(interval) != "5" {
			t.Fatalf("numeric interval = %s, want 5", interval)
		}
		if name == "start-string-interval.json" && string(interval) != `"5"` {
			t.Fatalf("string interval = %s, want \"5\"", interval)
		}
		if parsed, err := strconv.Atoi(strings.Trim(string(interval), `"`)); err != nil || parsed != 5 {
			t.Fatalf("parse interval %s = %d, %v", interval, parsed, err)
		}
	}
	if authInteger(t, decodeAuthFixture(t, "start-invalid-interval.json"), "interval") != 0 {
		t.Fatal("invalid interval fixture is no longer non-positive")
	}
	for _, name := range []string{"poll-pending.json", "poll-pending-403.json", "poll-pending-404.json"} {
		fields := decodeAuthFixture(t, name)
		if got := authString(t, fields, "error"); got != "authorization_pending" {
			t.Fatalf("%s error = %q", name, got)
		}
		if name == "poll-pending-403.json" && authInteger(t, fields, "status") != 403 || name == "poll-pending-404.json" && authInteger(t, fields, "status") != 404 {
			t.Fatalf("%s pending status mismatch", name)
		}
	}
	slowDown := decodeAuthFixture(t, "poll-slow-down.json")
	if authString(t, slowDown, "error") != "slow_down" || authInteger(t, slowDown, "interval") <= 5 {
		t.Fatal("slow-down fixture does not increase the interval")
	}
	authorized := decodeAuthFixture(t, "poll-authorized.json")
	authString(t, authorized, "authorization_code")
	authString(t, authorized, "code_verifier")

	for _, name := range []string{"exchange-success.json", "refresh-success.json", "refresh-no-rotation.json", "exchange-no-refresh.json"} {
		fields := decodeAuthFixture(t, name)
		authString(t, fields, "access_token")
		if authInteger(t, fields, "expires_in") != 3600 {
			t.Fatalf("%s expires_in mismatch", name)
		}
	}
	for _, name := range []string{"exchange-success.json", "refresh-success.json"} {
		authString(t, decodeAuthFixture(t, name), "refresh_token")
	}
	for _, name := range []string{"exchange-no-refresh.json", "refresh-no-rotation.json"} {
		if _, ok := decodeAuthFixture(t, name)["refresh_token"]; ok {
			t.Fatalf("%s unexpectedly contains refresh_token", name)
		}
	}
	for _, name := range []string{"exchange-terminal-error.json", "refresh-terminal-error.json"} {
		fields := decodeAuthFixture(t, name)
		if authString(t, fields, "error") != "invalid_grant" || len(authString(t, fields, "error_description")) > 128 {
			t.Fatalf("%s terminal error is not bounded", name)
		}
	}
	testAuthFixtureManifestRejectsMalformedAndDuplicateRows(t)
}

func testAuthFixtureManifestRejectsMalformedAndDuplicateRows(t *testing.T) {
	t.Helper()
	validHeader := "| File | SHA-256 |\n| --- | --- |\n"
	validRow := "| `one.json` | `" + strings.Repeat("a", 64) + "` |\n"
	for name, manifest := range map[string]string{
		"malformed-row":   validHeader + "| `one.json` | not-a-hash |\n",
		"malformed-shape": validHeader + "| `one.json` | `" + strings.Repeat("a", 64) + "` | extra |\n",
		"duplicate-row":   validHeader + validRow + validRow,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseAuthFixtureHashes([]byte(manifest)); err == nil {
				t.Fatal("malformed manifest unexpectedly accepted")
			}
		})
	}
}

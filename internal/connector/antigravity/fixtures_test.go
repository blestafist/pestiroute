package antigravity

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestFixturesIntegrity(t *testing.T) {
	const root = "testdata"
	wantFiles := []string{
		"request-positive.json", "stream-text.sse", "stream-usage.sse",
		"stream-premature-eof.sse", "stream-malformed.sse", "stream-function-calls.sse", "http-401.json",
		"http-403.json", "http-429.json", "http-500.json", "oauth-refresh-rotated.json",
		"onboard-load-eligible-free-tier.json", "onboard-load-rejected-terms-false.json",
		"onboard-load-rejected-user-project.json", "onboard-load-rejected-paid-tier.json",
		"onboard-load-rejected-current-tier.json", "onboard-response-lro-progress.json",
		"onboard-poll-terminal-success.json",
	}
	manifestBytes, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Version           int               `json:"version"`
		Provenance        string            `json:"provenance"`
		SourceObservation string            `json:"source_observation"`
		ProviderCapture   bool              `json:"provider_capture"`
		License           string            `json:"license"`
		Files             map[string]string `json:"files"`
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Version != 1 || !strings.Contains(manifest.Provenance, "Synthetic") || manifest.SourceObservation != "Rahularya01/opencode-antigravity@874ef4d6cc44f852401e6298790c6b34804e2978" || manifest.ProviderCapture || manifest.License != "MIT" || len(manifest.Files) != len(wantFiles) {
		t.Fatalf("invalid synthetic fixture manifest: %+v", manifest)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if entry.Name() == "manifest.json" {
			continue
		}
		if entry.IsDir() {
			t.Errorf("unexpected fixture entry %q", entry.Name())
			continue
		}
		want, ok := manifest.Files[entry.Name()]
		if !ok {
			t.Errorf("unregistered fixture %q", entry.Name())
			continue
		}
		seen[entry.Name()] = true
		data, err := os.ReadFile(filepath.Join(root, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Errorf("SHA-256 %s = %s, want %s", entry.Name(), got, want)
		}
		checkSyntheticContent(t, entry.Name(), data)
	}
	for _, name := range wantFiles {
		if !seen[name] {
			t.Errorf("required fixture %q missing or unregistered", name)
		}
	}
	checkFixtureSemantics(t, root)
}

var fixtureURL = regexp.MustCompile(`https?://[^\s"'<>]+`)

func checkSyntheticContent(t *testing.T, name string, data []byte) {
	t.Helper()
	content := string(data)
	lower := strings.ToLower(content)
	markers := []string{"bearer ", "access_token", "refresh_token", "client_secret", "api_key", "authorization:"}
	if name == "oauth-refresh-rotated.json" {
		markers = []string{"bearer ", "client_secret", "api_key", "authorization:"}
		if !strings.Contains(content, "SYNTHETIC_") {
			t.Errorf("%s lacks conspicuously synthetic token values", name)
		}
	}
	for _, marker := range markers {
		if strings.Contains(lower, marker) {
			t.Errorf("%s contains credential-like marker %q", name, marker)
		}
	}
	for _, raw := range fixtureURL.FindAllString(content, -1) {
		u, err := url.Parse(raw)
		if err != nil || !strings.HasSuffix(u.Hostname(), ".invalid") {
			t.Errorf("%s contains non-synthetic URL %q", name, raw)
		}
	}
}

func checkFixtureSemantics(t *testing.T, root string) {
	t.Helper()
	read := func(name string) []byte {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	var refresh struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(read("oauth-refresh-rotated.json"), &refresh); err != nil || refresh.AccessToken != "SYNTHETIC_ROTATED_ACCESS_TOKEN" || refresh.RefreshToken != "SYNTHETIC_ROTATED_REFRESH_TOKEN" || refresh.ExpiresIn <= 0 {
		t.Errorf("invalid synthetic OAuth refresh response: %+v (%v)", refresh, err)
	}
	var request struct {
		Project string `json:"project"`
		Model   string `json:"model"`
		Request struct {
			Contents []struct {
				Role  string `json:"role"`
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"contents"`
		} `json:"request"`
	}
	if err := json.Unmarshal(read("request-positive.json"), &request); err != nil || request.Project != "synthetic-project" || (request.Model != "gemini-2.5-pro" && request.Model != "gemini-2.5-flash") || len(request.Request.Contents) != 1 || request.Request.Contents[0].Role != "user" || len(request.Request.Contents[0].Parts) != 1 || !strings.Contains(request.Request.Contents[0].Parts[0].Text, "Synthetic") {
		t.Fatalf("invalid candidate synthetic request: %+v (%v)", request, err)
	}

	text := read("stream-text.sse")
	if strings.Count(string(text), "data: ") != 2 || !strings.Contains(string(text), "Synthetic ") || !strings.Contains(string(text), "stream text.") {
		t.Errorf("text stream does not contain two incremental synthetic text chunks")
	}
	toolStream := read("stream-function-calls.sse")
	if strings.Count(string(toolStream), `"functionCall"`) != 2 || !strings.Contains(string(toolStream), `"id":"candidate-call-a"`) || !strings.Contains(string(toolStream), `"id":"candidate-call-b"`) {
		t.Errorf("function-call fixture lacks two distinct candidate calls")
	}
	usage := read("stream-usage.sse")
	if !strings.Contains(string(usage), `"promptTokenCount":7`) || !strings.Contains(string(usage), `"candidatesTokenCount":4`) || !strings.Contains(string(usage), `"done":true`) {
		t.Errorf("usage stream lacks fixture usage or its declared terminal marker")
	}
	if strings.Contains(string(read("stream-premature-eof.sse")), `"done":true`) || strings.HasSuffix(string(read("stream-premature-eof.sse")), "\n\n") {
		t.Errorf("premature EOF fixture unexpectedly contains a terminal event")
	}
	if json.Valid([]byte(strings.TrimSpace(strings.TrimPrefix(string(read("stream-malformed.sse")), "data: ")))) {
		t.Errorf("malformed stream fixture unexpectedly contains valid JSON")
	}

	for status, want := range map[int]string{401: "UNAUTHENTICATED", 403: "PERMISSION_DENIED", 429: "RESOURCE_EXHAUSTED", 500: "INTERNAL"} {
		var payload struct {
			Error struct {
				Code   int    `json:"code"`
				Status string `json:"status"`
			} `json:"error"`
		}
		name := fmt.Sprintf("http-%d.json", status)
		if err := json.Unmarshal(read(name), &payload); err != nil || payload.Error.Code != status || payload.Error.Status != want {
			t.Errorf("%s has error code/status %d/%q, want %d/%q (err %v)", name, payload.Error.Code, payload.Error.Status, status, want, err)
		}
	}
}

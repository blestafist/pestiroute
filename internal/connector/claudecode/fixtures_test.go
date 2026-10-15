package claudecode

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestFixturesIntegrity validates synthetic examples only; passing is not provider proof.
func TestFixturesIntegrity(t *testing.T) {
	root := "testdata"
	manifestBytes, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Version    int               `json:"version"`
		Provenance string            `json:"provenance"`
		Files      map[string]string `json:"files"`
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Version != 1 || !strings.Contains(manifest.Provenance, "synthetic") || len(manifest.Files) == 0 {
		t.Fatalf("invalid fixture manifest: %+v", manifest)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() == "README.md" || entry.Name() == "manifest.json" {
			continue
		}
		want, ok := manifest.Files[entry.Name()]
		if !ok {
			t.Errorf("fixture %q is not registered in manifest", entry.Name())
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
	}
	for name := range manifest.Files {
		if !seen[name] {
			t.Errorf("manifest fixture %q is missing", name)
		}
	}
	checkFixtureContent(t, root)
}

func checkFixtureContent(t *testing.T, root string) {
	t.Helper()
	read := func(name string) []byte {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	authorize := strings.TrimSpace(string(read("oauth-authorize.url")))
	authorizeURL, err := url.Parse(authorize)
	if err != nil || authorizeURL.Host != "auth.synthetic.invalid" || authorizeURL.Query().Get("code_challenge_method") != "S256" || authorizeURL.Query().Get("state") != "SYNTHETIC_STATE" {
		t.Fatalf("authorize URL is not synthetic PKCE: %q (err %v)", authorize, err)
	}
	for _, name := range []string{"oauth-exchange.json", "oauth-exchange-response.json", "oauth-refresh.json", "oauth-refresh-response.json", "messages-request.json", "http-400.json", "http-401.json", "http-403.json", "http-429.json", "http-500.json"} {
		if !json.Valid(read(name)) {
			t.Errorf("%s is not valid JSON", name)
		}
	}
	for name, markers := range map[string][]string{
		"oauth-exchange.json":          {`"grant_type":"authorization_code"`, `"code_verifier":"SYNTHETIC_VERIFIER"`},
		"oauth-exchange-response.json": {`"access_token":"SYNTHETIC_ACCESS_TOKEN"`, `"refresh_token":"SYNTHETIC_REFRESH_TOKEN"`},
		"oauth-refresh.json":           {`"grant_type":"refresh_token"`, `"refresh_token":"SYNTHETIC_REFRESH_TOKEN"`},
		"oauth-refresh-response.json":  {`"access_token":"SYNTHETIC_ROTATED_ACCESS_TOKEN"`},
	} {
		for _, marker := range markers {
			if !bytes.Contains(read(name), []byte(marker)) {
				t.Errorf("%s missing expected synthetic OAuth field %s", name, marker)
			}
		}
	}
	var request struct {
		Stream   bool `json:"stream"`
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(read("messages-request.json"), &request); err != nil || !request.Stream || len(request.Messages) == 0 || len(request.Tools) == 0 {
		t.Fatalf("synthetic Messages request incomplete: %+v (%v)", request, err)
	}
	for name, want := range map[string][]string{
		"stream-text.sse":     {"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"},
		"stream-tools.sse":    {"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"},
		"stream-thinking.sse": {"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"},
		"stream-usage.sse":    {"message_start", "message_delta", "message_stop"},
	} {
		data := read(name)
		got, err := parseFixtureSSE(data)
		if err != nil {
			t.Errorf("%s: %v", name, err)
		} else if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s events = %v, want permitted sequence %v", name, got, want)
		}
	}
	for name, markers := range map[string][]string{
		"stream-text.sse":     {"text_delta", "Synthetic hello."},
		"stream-tools.sse":    {"tool_use", "input_json_delta", `"stop_reason":"tool_use"`},
		"stream-thinking.sse": {"thinking_delta", "Synthetic reasoning."},
		"stream-usage.sse":    {`"input_tokens":23`, `"output_tokens":9`, `"cache_read_input_tokens":5`},
		"stream-error.sse":    {`"type":"error"`, "overloaded_error"},
	} {
		data := read(name)
		if name == "stream-error.sse" {
			if _, err := parseFixtureSSE(data); err != nil {
				t.Errorf("%s: %v", name, err)
			}
		}
		for _, marker := range markers {
			if !bytes.Contains(data, []byte(marker)) {
				t.Errorf("%s missing expected stream content %q", name, marker)
			}
		}
	}
	if _, err := parseFixtureSSE(read("stream-malformed.sse")); err == nil {
		t.Error("malformed SSE fixture unexpectedly parsed")
	}
	if utf8.Valid(read("stream-invalid-utf8.sse")) {
		t.Error("invalid UTF-8 fixture contains only valid UTF-8")
	}
	for _, name := range []string{"stream-premature-eof.sse", "stream-midstream-disconnect.sse"} {
		data := read(name)
		if bytes.HasSuffix(data, []byte("\n\n")) {
			t.Errorf("%s unexpectedly terminates its final SSE event", name)
		}
		if bytes.Contains(data, []byte(`"type":"message_stop"`)) {
			t.Errorf("%s unexpectedly has a successful terminal event", name)
		}
	}
	for status, wantType := range map[string]string{
		"400": "invalid_request_error",
		"401": "authentication_error",
		"403": "permission_error",
		"429": "rate_limit_error",
		"500": "api_error",
	} {
		var payload struct {
			Type  string `json:"type"`
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		if err := json.Unmarshal(read("http-"+status+".json"), &payload); err != nil || payload.Type != "error" || payload.Error.Type != wantType {
			t.Errorf("HTTP %s error fixture = type %q error.type %q, want error/%q (err %v)", status, payload.Type, payload.Error.Type, wantType, err)
		}
	}
}

func parseFixtureSSE(data []byte) ([]string, error) {
	if !utf8.Valid(data) {
		return nil, os.ErrInvalid
	}
	var events []string
	for _, frame := range strings.Split(string(data), "\n\n") {
		if strings.TrimSpace(frame) == "" {
			continue
		}
		var event string
		var payload strings.Builder
		for _, line := range strings.Split(frame, "\n") {
			if value, ok := strings.CutPrefix(line, "event: "); ok {
				event = value
			}
			if value, ok := strings.CutPrefix(line, "data: "); ok {
				payload.WriteString(value)
			}
		}
		var decoded struct {
			Type string `json:"type"`
		}
		if event == "" || payload.Len() == 0 || json.Unmarshal([]byte(payload.String()), &decoded) != nil || decoded.Type != event {
			return nil, os.ErrInvalid
		}
		events = append(events, event)
	}
	if len(events) == 0 {
		return nil, os.ErrInvalid
	}
	return events, nil
}

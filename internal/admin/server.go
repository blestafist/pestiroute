// Package admin provides the isolated HTTP boundary for the management API.
package admin

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"slices"
	"strings"
	"time"

	secure "github.com/blestafist/pestiroute/internal/crypto"
)

type Component struct {
	ID             string            `json:"id"`
	Kind           string            `json:"kind"`
	Implementation string            `json:"implementation"`
	Health         string            `json:"health"`
	Protocols      []string          `json:"protocols"`
	Capabilities   map[string]string `json:"capabilities"`
}
type Route struct {
	Protocol    string `json:"protocol"`
	Mode        string `json:"mode"`
	Model       string `json:"model"`
	AdapterID   string `json:"adapter_id"`
	ConnectorID string `json:"connector_id"`
	AccountID   string `json:"account_id"`
}

type Config struct {
	Address                                string
	TokenFile                              string
	TokenEnv                               string
	TLSCertFile, TLSKeyFile                string
	AllowedHosts                           []string
	AllowedOrigins                         []string
	MaxBodyBytes                           int64
	ReadTimeout, WriteTimeout, IdleTimeout time.Duration
	Static                                 http.Handler
	DB                                     *sql.DB
	MasterKey                              secure.MasterKey
	Version, Readiness                     string
	Components                             []Component
	Routes                                 []Route
}

type rawBodyKey struct{}

// New validates operator credentials and returns the authenticated API handler.
func New(c Config) (http.Handler, error) {
	host, _, err := net.SplitHostPort(c.Address)
	if err != nil || strings.TrimSpace(c.Address) == "" {
		return nil, fmt.Errorf("admin address must be host:port")
	}
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		return nil, fmt.Errorf("admin TLS certificate and key must be configured together")
	}
	if c.TLSCertFile != "" {
		if _, err := tls.LoadX509KeyPair(c.TLSCertFile, c.TLSKeyFile); err != nil {
			return nil, fmt.Errorf("invalid admin TLS certificate or key")
		}
	}
	if c.TokenFile != "" && c.TokenEnv != "" {
		return nil, fmt.Errorf("configure exactly one admin token source")
	}
	if host != "127.0.0.1" && host != "::1" {
		if c.TLSCertFile == "" || c.TLSKeyFile == "" {
			return nil, fmt.Errorf("non-loopback admin bind requires TLS")
		}
	}
	var token string
	if c.TokenFile != "" {
		file, err := os.Open(c.TokenFile)
		if err != nil {
			return nil, fmt.Errorf("admin token file unavailable")
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil || info.Mode().Perm()&0077 != 0 || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("admin token file must be regular and accessible only by its owner")
		}
		data, err := io.ReadAll(io.LimitReader(file, 64<<10+1))
		if err != nil {
			return nil, fmt.Errorf("admin token file unavailable")
		}
		if len(data) > 64<<10 {
			return nil, fmt.Errorf("admin token file is too large")
		}
		token = strings.TrimSpace(string(data))
	} else if c.TokenEnv != "" {
		token = os.Getenv(c.TokenEnv)
	}
	if !validBearerToken(token) {
		return nil, fmt.Errorf("admin operator token must be a valid bearer token")
	}
	if c.MaxBodyBytes < 0 {
		return nil, fmt.Errorf("admin max body size must not be negative")
	}
	if c.MaxBodyBytes > int64(int(^uint(0)>>1))-1 {
		return nil, fmt.Errorf("admin max body size is too large")
	}
	if c.MaxBodyBytes == 0 {
		c.MaxBodyBytes = 64 << 10
	}
	if c.ReadTimeout <= 0 || c.WriteTimeout <= 0 || c.IdleTimeout <= 0 {
		return nil, fmt.Errorf("admin timeouts must be positive")
	}
	allowedHosts := []string{c.Address, host}
	for _, proxyHost := range c.AllowedHosts {
		if !contains(allowedHosts, proxyHost) {
			allowedHosts = append(allowedHosts, proxyHost)
		}
	}
	api := http.NewServeMux()
	registerReadHandlers(api, c)
	registerAccountHandlers(api, c)
	registerPolicyKeyHandlers(api, c)
	api.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { writeError(w, 404, "not_found", "Not found") })
	root := http.NewServeMux()
	apiHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !contains(allowedHosts, r.Host) {
			writeError(w, 403, "forbidden", "Forbidden")
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			if origin == "null" || !allowedOrigin(origin, r, c.AllowedOrigins) {
				writeError(w, 403, "forbidden", "Forbidden")
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		authorization := r.Header.Values("Authorization")
		var got string
		if len(authorization) == 1 {
			got = bearerToken(authorization[0])
		}
		if len(got) != len(token) || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			writeError(w, 401, "unauthorized", "Unauthorized")
			return
		}
		bodyLimit := c.MaxBodyBytes
		if strings.HasPrefix(r.URL.Path, "/admin/v1/accounts/") && strings.Contains(r.URL.Path, "/credentials") {
			bodyLimit = max(bodyLimit, int64(6*(64<<10)+4096))
		}
		defer r.Body.Close()
		dispatchBody(w, r, api, bodyLimit)
	})
	root.Handle("/admin/v1", apiHandler)
	root.Handle("/admin/v1/", apiHandler)
	root.Handle("/admin/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !contains(allowedHosts, r.Host) {
			writeError(w, 403, "forbidden", "Forbidden")
			return
		}
		if c.Static == nil {
			http.NotFound(w, r)
			return
		}
		if strings.HasPrefix(path.Clean(r.URL.Path), "/admin/v1") {
			http.NotFound(w, r)
			return
		}
		c.Static.ServeHTTP(w, r)
	}))
	return root, nil
}

func dispatchBody(w http.ResponseWriter, r *http.Request, api http.Handler, limit int64) {
	data, oversized, err := readBoundedBody(r.Body, limit)
	if err != nil {
		zeroBuffer(data)
		writeError(w, 400, "invalid_request", "Invalid request")
		return
	}
	defer zeroBuffer(data)
	if oversized {
		writeError(w, 413, "payload_too_large", "Payload too large")
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	r = r.WithContext(context.WithValue(r.Context(), rawBodyKey{}, &data))
	api.ServeHTTP(w, r)
}

// readBoundedBody grows one tracked buffer; every superseded allocation is
// cleared before it is released, and the caller clears the returned allocation.
func readBoundedBody(reader io.Reader, limit int64) ([]byte, bool, error) {
	maxSize := int(limit + 1)
	initial := min(maxSize, 4096)
	buffer := make([]byte, initial)
	used := 0
	for {
		if used == len(buffer) {
			if used == maxSize {
				return buffer[:used], true, nil
			}
			nextSize := min(maxSize, max(len(buffer)*2, 1))
			next := make([]byte, nextSize)
			copy(next, buffer[:used])
			clear(buffer)
			buffer = next
		}
		n, err := reader.Read(buffer[used:])
		used += n
		if int64(used) > limit {
			return buffer[:used], true, nil
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return buffer[:used], false, nil
			}
			return buffer[:used], false, err
		}
		if n == 0 {
			return buffer[:used], false, io.ErrNoProgress
		}
	}
}

func zeroBuffer(buffer []byte) { clear(buffer[:cap(buffer)]) }

func contains(values []string, value string) bool { return slices.Contains(values, value) }

func bearerToken(value string) string {
	scheme, token, ok := strings.Cut(value, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || !validBearerToken(token) {
		return ""
	}
	return token
}

func validBearerToken(token string) bool {
	if token == "" {
		return false
	}
	padding := false
	content := false
	for i := 0; i < len(token); i++ {
		c := token[i]
		if c == '=' {
			if !content {
				return false
			}
			padding = true
			continue
		}
		if padding || !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-._~+/", rune(c))) {
			return false
		}
		content = true
	}
	return true
}

func allowedOrigin(origin string, r *http.Request, configured []string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	if strings.EqualFold(u.Host, r.Host) && strings.EqualFold(u.Scheme, requestScheme(r)) {
		return true
	}
	return contains(configured, origin)
}
func requestScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}
func writeError(w http.ResponseWriter, status int, code, message string) {
	var requestID [16]byte
	_, _ = rand.Read(requestID[:])
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": message, "request_id": hex.EncodeToString(requestID[:])}})
}

package main

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	adapter "github.com/blestafist/pestiroute/internal/adapter/responses"
	"github.com/blestafist/pestiroute/internal/connector/anthropic"
	"github.com/blestafist/pestiroute/internal/connector/codex"
	responses "github.com/blestafist/pestiroute/internal/connector/responses"
	"github.com/blestafist/pestiroute/internal/core"
)

// TestM51LocalDaemon is a test-binary-only composition root for LOCAL-M5.1.
// Production Codex deliberately pins its endpoint and disables environment
// proxies; this helper substitutes only the test process's HTTPS proxy client.
func TestM51LocalDaemon(t *testing.T) {
	if os.Getenv("PESTIROUTE_M51_LOCAL_DAEMON") != "1" {
		t.Skip("local operations helper only")
	}
	path, dbPath, keyPath := os.Getenv("PESTIROUTE_M51_CONFIG"), os.Getenv("PESTIROUTE_M51_DB"), os.Getenv("PESTIROUTE_M51_KEY")
	if path == "" || dbPath == "" || keyPath == "" {
		t.Fatal("local operations helper configuration is incomplete")
	}
	protected, err := loadProtectedYAML(path)
	if err != nil {
		t.Fatal(err)
	}
	anthropicTarget, err := url.Parse(os.Getenv("PESTIROUTE_M51_ANTHROPIC_URL"))
	if err != nil || anthropicTarget.Scheme != "http" || net.ParseIP(anthropicTarget.Hostname()) == nil ||
		!net.ParseIP(anthropicTarget.Hostname()).IsLoopback() || anthropicTarget.Port() == "" ||
		anthropicTarget.Path != "/v1/messages" || anthropicTarget.User != nil || anthropicTarget.RawQuery != "" || anthropicTarget.Fragment != "" {
		t.Fatal("local Anthropic fixture must be a loopback HTTP endpoint")
	}
	prepared, err := prepareProtectedConfig(context.Background(), config{protected: &protected, DatabasePath: dbPath, MasterKeyFile: keyPath})
	if err != nil {
		t.Fatal(err)
	}
	var ready, draining atomic.Bool
	ready.Store(true)
	handler, closeComponents, err := composeHandlerWithFactory(prepared, &ready, &draining, nil, func(item topologyComponent) core.Component {
		if item.Kind == core.ComponentAdapter {
			return adapter.NewAdapter()
		}
		switch item.Implementation {
		case "pestiroute.codex.responses":
			client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}}
			return codexCompositionConnector{Connector: codex.NewConnector(), doer: client}
		case "pestiroute.anthropic.messages":
			client := &http.Client{Transport: &http.Transport{}}
			doer := anthropicDoerFunc(func(req *http.Request) (*http.Response, error) {
				copy := req.Clone(req.Context())
				copy.URL = anthropicTarget
				copy.Host = anthropicTarget.Host
				return client.Do(copy)
			})
			return anthropicConnectorWithDoer{Connector: anthropic.NewConnector(), doer: doer}
		default:
			return responses.NewConnector()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeComponents(context.Background())
	listener, err := net.Listen("tcp", protected.Server.Listen)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-signals:
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-serveErr; err != nil && err != http.ErrServerClosed {
			t.Fatal(err)
		}
	case err := <-serveErr:
		if err != nil && err != http.ErrServerClosed {
			t.Fatal(err)
		}
	}
}

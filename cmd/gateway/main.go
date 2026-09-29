package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

type config struct {
	Listen          string `json:"listen"`
	ShutdownTimeout string `json:"shutdown_timeout"`
}

func loadConfig(args []string) (config, time.Duration, error) {
	flags := flag.NewFlagSet("gateway", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("config", "", "JSON configuration file")
	listen := flags.String("listen", "", "listener address")
	timeout := flags.String("shutdown-timeout", "", "graceful shutdown duration")
	if err := flags.Parse(args); err != nil {
		return config{}, 0, err
	}
	if flags.NArg() != 0 {
		return config{}, 0, fmt.Errorf("unexpected arguments: %v", flags.Args())
	}
	c := config{Listen: "127.0.0.1:8080", ShutdownTimeout: "5s"}
	if *path != "" {
		file, err := os.Open(*path)
		if err != nil {
			return config{}, 0, fmt.Errorf("config %q: %w", *path, err)
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
		if err != nil {
			return config{}, 0, fmt.Errorf("config %q: %w", *path, err)
		}
		if len(data) > 1<<20 {
			return config{}, 0, fmt.Errorf("config %q: exceeds 1 MiB", *path)
		}
		if trimmed := bytes.TrimSpace(data); len(trimmed) == 0 || trimmed[0] != '{' {
			return config{}, 0, fmt.Errorf("config %q: expected JSON object", *path)
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&c); err != nil {
			return config{}, 0, fmt.Errorf("config %q: %w", *path, err)
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			return config{}, 0, fmt.Errorf("config %q: expected one JSON object (trailing data or oversized file): %v", *path, err)
		}
	}
	if *listen != "" {
		c.Listen = *listen
	}
	if *timeout != "" {
		c.ShutdownTimeout = *timeout
	}
	if strings.TrimSpace(c.Listen) == "" {
		return config{}, 0, errors.New("listen must be a non-empty host:port address")
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return config{}, 0, fmt.Errorf("invalid listen address %q: %w", c.Listen, err)
	}
	d, err := time.ParseDuration(c.ShutdownTimeout)
	if err != nil || d <= 0 {
		return config{}, 0, fmt.Errorf("invalid shutdown_timeout %q: must be a positive duration", c.ShutdownTimeout)
	}
	return c, d, nil
}

func probes(ready *atomic.Bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	return mux
}

func run(ctx context.Context, args []string) error {
	c, timeout, err := loadConfig(args)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return fmt.Errorf("listen %q: %w", c.Listen, err)
	}
	var ready atomic.Bool
	server := &http.Server{Handler: probes(&ready), ReadHeaderTimeout: 5 * time.Second}
	done := make(chan error, 1)
	go func() {
		ready.Store(true)
		done <- server.Serve(listener)
	}()
	fmt.Fprintf(os.Stderr, "gateway: listening on %s\n", listener.Addr())
	select {
	case err := <-done:
		ready.Store(false)
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
		ready.Store(false)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			server.Close()
			<-done
			return fmt.Errorf("shutdown: %w", err)
		}
		<-done
		return nil
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "gateway:", err)
		os.Exit(1)
	}
}

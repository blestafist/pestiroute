//go:build linux

package main

import (
	"context"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/blestafist/pestiroute/internal/connector/external"
	"github.com/blestafist/pestiroute/internal/connector/responses"
	"github.com/blestafist/pestiroute/internal/core"
)

type uncooperativeConnector struct{ core.Connector }

func (c *uncooperativeConnector) Close(ctx context.Context) error {
	timer := time.NewTimer(500 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		return ctx.Err()
	}
	return c.Connector.Close(ctx)
}

func main() {
	if len(os.Args) != 2 {
		os.Exit(2)
	}
	if err := os.WriteFile(os.Args[1], []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	_ = external.RunConnectorChild(ctx, &uncooperativeConnector{Connector: responses.NewConnector()})
	for {
		time.Sleep(time.Second)
	}
}

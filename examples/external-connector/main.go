//go:build linux

// This example must be built inside the PestiRoute module: the M6 Go helpers
// are internal packages, not a separately versioned SDK.
package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"

	"github.com/blestafist/pestiroute/internal/connector/external"
	"github.com/blestafist/pestiroute/internal/connector/responses"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// Replace this implementation with a core.Connector in an in-tree extension.
	// The helper owns wire framing, reverse services, and process lifecycle.
	if err := external.RunConnectorChild(ctx, responses.NewConnector()); err != nil {
		log.Fatalf("external connector example: %v", err)
	}
}

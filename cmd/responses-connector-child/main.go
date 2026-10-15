//go:build linux

package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"

	"github.com/blestafist/pestiroute/internal/connector/external"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := external.RunNativeResponsesChild(ctx); err != nil {
		log.Fatalf("Responses connector child failed: %v", err)
	}
}

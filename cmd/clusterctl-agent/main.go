// Command clusterctl-agent watches a Kubernetes cluster for problems and sends
// alerts to clusterctl on your phone. See README.md.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/RazaLabs/clusterctl-agent/internal/agent"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	cfg, err := agent.FromEnv()
	if err != nil {
		slog.Error("configuration", "err", err)
		os.Exit(1)
	}
	a, err := agent.New(ctx, cfg)
	if err != nil {
		slog.Error("starting", "err", err)
		os.Exit(1)
	}
	if err := a.Run(ctx); err != nil {
		slog.Error("stopped", "err", err)
		os.Exit(1)
	}
}

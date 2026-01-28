package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"
)

var (
	Version   = "0.1.0"
	GitCommit = "main"
	BuildDate = "20260128"
)

func main() {
	cfg := NewConfig()
	flag.Parse()

	logger := cfg.Logger()

	mode := "dynamic"
	if cfg.WowzaWSURL != "" {
		mode = "static"
	}

	logger.Info("starting wowza2whep",
		"version", Version,
		"listen", cfg.ListenAddr,
		"mode", mode,
	)

	if mode == "static" {
		logger.Info("using static Wowza URL", "url", cfg.WowzaWSURL)
	}

	mgr := NewManager(cfg, logger)
	srv := NewServer(cfg, mgr, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(ctx) }()

	select {
	case sig := <-sigCh:
		logger.Info("shutdown signal received", "signal", sig.String())
		cancel()
		if err := <-errCh; err != nil {
			logger.Error("server shutdown error", "error", err)
			os.Exit(1)
		}
	case err := <-errCh:
		if err != nil {
			logger.Error("server error", "error", err)
			os.Exit(1)
		}
	}

	logger.Info("shutdown complete")
}

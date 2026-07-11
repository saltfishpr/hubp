package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/saltfishpr/hubp/internal/config"
	"github.com/saltfishpr/hubp/internal/proxy"
)

func main() {
	configPath := flag.String("config", "config.json", "path to JSON configuration file")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("load configuration failed", "error", err)
		os.Exit(1)
	}
	handler, err := proxy.NewHandler(cfg, logger)
	if err != nil {
		logger.Error("initialize proxy failed", "error", err)
		os.Exit(1)
	}

	server := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           handler,
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout.Value(),
		IdleTimeout:       cfg.Server.IdleTimeout.Value(),
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		ctx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout.Value())
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			logger.Error("graceful shutdown failed", "error", err)
		}
	}()

	logger.Info("hubp started", "listen", cfg.Server.Listen, "cache_enabled", cfg.Cache.Enabled, "cache_dir", cfg.Cache.Dir)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("HTTP server failed", "error", err)
		os.Exit(1)
	}
	fmt.Println("hubp stopped")
}

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"hubp/internal/cache"
	"hubp/internal/config"
	"hubp/internal/server"
	"hubp/internal/transfer"
	"hubp/internal/upstream"
)

func main() {
	if err := run(); err != nil {
		slog.Error("hubp stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	file := flag.String("config", "", "JSON configuration file; defaults to local development hosts")
	flag.Parse()

	cfg, err := config.Load(*file)
	if err != nil {
		return err
	}
	if value, ok := os.LookupEnv("HUBP_UPSTREAM_PROXY"); ok {
		cfg.UpstreamProxy = value
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return serve(ctx, cfg)
}

func serve(ctx context.Context, cfg config.Config) (err error) {
	proxy, err := config.ProxyURL(cfg.UpstreamProxy)
	if err != nil {
		return err
	}
	client := upstream.NewWithProxy(cfg.HeaderTimeout(), cfg.StallTimeout(), cfg.MaxUpstreamRequests, proxy)
	defer client.Close()
	delivery := &transfer.Service{WriteTimeout: cfg.StallTimeout()}
	if cfg.Cache.Enabled {
		store, openErr := cache.Open(cache.Config{
			Dir:            cfg.Cache.Dir,
			MaxBytes:       cfg.Cache.MaxBytes,
			MaxObjectBytes: cfg.Cache.MaxObjectBytes,
			MinFreeBytes:   cfg.Cache.MinFreeBytes,
		})
		if openErr != nil {
			return fmt.Errorf("open cache: %w", openErr)
		}
		delivery.Cache = store

		// 维护任务在 HTTP 请求全部退出后停止，不直接跟随退出信号取消。
		maintenance, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			store.Maintain(maintenance)
		}()
		defer func() {
			cancel()
			<-done
			if closeErr := store.Close(); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("close cache: %w", closeErr))
			}
		}()
	}
	router, err := server.NewRouter(cfg, client, delivery)
	if err != nil {
		return err
	}
	return serveHTTP(ctx, cfg.Listen, router, cfg.Cache.Enabled)
}

func serveHTTP(ctx context.Context, addr string, router http.Handler, cacheEnabled bool) error {
	// 退出信号只触发排空，宽限期结束后才取消尚未完成的请求。
	requests, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()

	var active sync.WaitGroup
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		active.Add(1)
		defer active.Done()
		router.ServeHTTP(w, r)
	})
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	srv.BaseContext = func(_ net.Listener) context.Context { return requests }
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	defer func() {
		cancelRequests()
		_ = srv.Close()
		active.Wait()
	}()

	failed := make(chan error, 1)
	go func() { failed <- srv.Serve(listener) }()

	slog.Info("hubp started", "listen", listener.Addr().String(), "cache", cacheEnabled)
	select {
	case err := <-failed:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	}
}

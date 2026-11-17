// Command core is the call-audio analysis service.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"agent-security/internal/config"
	"agent-security/internal/engine"
	"agent-security/internal/plugin"
	"agent-security/internal/retention"
	"agent-security/internal/server"
	"agent-security/internal/store"
	_ "agent-security/plugins"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel()}))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.AudioDir, 0o700); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()

	plugins := plugin.All()
	names := make([]string, len(plugins))
	for i, p := range plugins {
		names[i] = p.Name()
	}
	log.Info("plugins loaded", "plugins", names)

	eng := engine.New(plugins, cfg.PluginTimeout, log)
	srv := server.New(cfg, eng, db, log)

	cleanCtx, stopClean := context.WithCancel(context.Background())
	cleanDone := make(chan struct{})
	go func() {
		defer close(cleanDone)
		(&retention.Cleaner{Store: db, Interval: cfg.CleanupInterval, Log: log}).Run(cleanCtx)
	}()

	httpSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.ListenAddr, "retention", cfg.Retention.String())
		errCh <- httpSrv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		stopClean()
		<-cleanDone
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	// Stop accepting new connections, then end in-flight calls and wait for
	// their results to be saved. (Hijacked WebSocket connections are not
	// tracked by http.Server, so the call server handles those itself.)
	if err := httpSrv.Shutdown(shutCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Warn("http shutdown", "err", err)
	}
	if err := srv.Shutdown(shutCtx); err != nil {
		log.Warn("in-flight calls did not finish before shutdown timeout", "err", err)
	}
	stopClean()
	<-cleanDone
	return nil
}

func logLevel() slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(os.Getenv("LOG_LEVEL"))); err != nil {
		return slog.LevelInfo
	}
	return l
}

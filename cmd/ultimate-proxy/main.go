// Command ultimate-proxy runs the Open Responses gateway.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/boramuyar/ultimate-proxy/internal/config"
	"github.com/boramuyar/ultimate-proxy/internal/meter"
	"github.com/boramuyar/ultimate-proxy/internal/server"
	"github.com/boramuyar/ultimate-proxy/internal/store"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to the config file")
	healthcheck := flag.String("healthcheck", "", "GET this URL and exit 0 if it answers 200 (for container health checks)")
	flag.Parse()
	if *healthcheck != "" {
		resp, err := (&http.Client{Timeout: 3 * time.Second}).Get(*healthcheck)
		if err != nil || resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		os.Exit(0)
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(*configPath, log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(configPath string, log *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var st store.Store
	if cfg.DatabaseURL == "" {
		log.Warn("database_url is empty; keeping keys and usage in memory only")
		st = store.NewMemory()
	} else {
		pg, err := store.NewPostgres(ctx, cfg.DatabaseURL)
		if err != nil {
			return err
		}
		st = pg
	}
	defer st.Close()

	m := meter.New(st, cfg.Usage.QueueSize, cfg.Usage.BatchSize, cfg.Usage.FlushInterval, log)
	defer m.Close()

	srv, err := server.New(cfg, st, m, log)
	if err != nil {
		return err
	}
	if err := srv.Bootstrap(ctx); err != nil {
		return err
	}
	if err := srv.Prices().Reload(ctx); err != nil {
		return err
	}
	go srv.Prices().Run(ctx, 30*time.Second)
	if eng := srv.Insights(); eng != nil {
		if err := eng.Load(ctx); err != nil {
			return err
		}
		go eng.Run(ctx)
	}

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Listen)
		errc <- httpSrv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		log.Info("shutting down; waiting for in-flight requests")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}
	return nil
}

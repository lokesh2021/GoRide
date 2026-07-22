// Command server runs the GoRide HTTP API.
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

	"github.com/lokeshbm/goride/internal/config"
	"github.com/lokeshbm/goride/internal/drivers"
	"github.com/lokeshbm/goride/internal/httpapi"
	"github.com/lokeshbm/goride/internal/matching"
	"github.com/lokeshbm/goride/internal/quotes"
	"github.com/lokeshbm/goride/internal/rides"
	"github.com/lokeshbm/goride/internal/store"
)

func main() {
	cfg := config.Load()
	logger := newLogger(cfg.Env)
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.New(ctx, cfg)
	if err != nil {
		logger.Error("failed to connect to store", "error", err)
		os.Exit(1)
	}
	defer st.Close()

	quoteSvc := quotes.NewService(st, logger)
	rideSvc := rides.NewService(st, quoteSvc, logger)
	driverSvc := drivers.NewService(st, logger)
	matchEngine := matching.NewEngine(st, rideSvc, driverSvc, logger)

	// Wire the M3 seams into the ride service: kick matching immediately on a
	// new MATCHING ride, and re-add a released driver to the geo pool.
	rideSvc.MatchRequested = matchEngine.RequestMatch
	rideSvc.OnDriverReleased = func(ctx context.Context, driverID string) {
		if err := driverSvc.Release(ctx, driverID); err != nil {
			logger.Warn("driver release failed", "error", err, "driver_id", driverID)
		}
	}

	// Start the matching sweeper; it stops when ctx is cancelled on shutdown.
	matchEngine.Start(ctx)

	router := httpapi.NewRouter(httpapi.Deps{
		Health:  st,
		Store:   st,
		Quotes:  quoteSvc,
		Rides:   rideSvc,
		Drivers: driverSvc,
		Match:   matchEngine,
		Logger:  logger,
	})

	srv := &http.Server{
		Addr:    cfg.Addr,
		Handler: router,
	}

	go func() {
		logger.Info("server starting", "addr", cfg.Addr, "env", cfg.Env)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server failed", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	logger.Info("shutdown signal received")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
		os.Exit(1)
	}

	logger.Info("server stopped cleanly")
}

// newLogger emits JSON logs in prod-like envs and text logs in dev.
func newLogger(env string) *slog.Logger {
	if env == "dev" {
		return slog.New(slog.NewTextHandler(os.Stdout, nil))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, nil))
}

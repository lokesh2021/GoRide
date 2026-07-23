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
	"github.com/lokeshbm/goride/internal/events"
	"github.com/lokeshbm/goride/internal/httpapi"
	"github.com/lokeshbm/goride/internal/matching"
	"github.com/lokeshbm/goride/internal/observability"
	"github.com/lokeshbm/goride/internal/payments"
	"github.com/lokeshbm/goride/internal/quotes"
	"github.com/lokeshbm/goride/internal/rides"
	"github.com/lokeshbm/goride/internal/store"
	"github.com/lokeshbm/goride/internal/trips"
)

func main() {
	cfg := config.Load()
	logger := newLogger(cfg.Env)
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Constructed first (before the store) since the pgx pool's nrpgx5 tracer
	// and the router's transaction middleware both need it; nil (monitoring
	// disabled) flows through every seam below unchanged — see
	// internal/observability's doc comment for the nil-safety design.
	obs := observability.New(cfg, logger)
	defer obs.Shutdown(10 * time.Second)

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
	matchEngine.SetObservability(obs)
	tripSvc := trips.NewService(st, rideSvc, driverSvc, quoteSvc, logger)
	psp := payments.NewPSP(cfg.PSPWebhookURL, cfg.PSPSecret, logger)
	paymentSvc := payments.NewService(st, rideSvc, psp, cfg.PSPSecret, logger)
	paymentSvc.SetObservability(obs)

	// M5: real-time fan-out. The Publisher satisfies both rides.EventPublisher
	// and drivers.RidePublisher (structural interfaces), so one instance wires
	// into both domain services. The Hub is built with ctx (this process's
	// SIGINT/SIGTERM-cancelled root context) so that in-flight SSE streams are
	// cut on shutdown even if their client never disconnects — otherwise
	// srv.Shutdown below would block waiting for a connection that never goes
	// idle.
	eventPub := events.NewPublisher(st.Redis, logger)
	rideSvc.SetEventPublisher(eventPub)
	driverSvc.SetPublisher(eventPub)
	eventHub := events.NewHub(ctx, st.Redis, logger)

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
		Health:   st,
		Store:    st,
		Quotes:   quoteSvc,
		Rides:    rideSvc,
		Drivers:  driverSvc,
		Match:    matchEngine,
		Trips:    tripSvc,
		Payments: paymentSvc,
		Events:   eventHub,
		Logger:   logger,
		Obs:      obs,
	})

	srv := &http.Server{
		Addr:    cfg.Addr,
		Handler: router,
		// ReadHeaderTimeout guards against slow-header (slowloris) connections;
		// it only bounds reading the request line + headers, so it cannot cut
		// off the SSE endpoints below. Deliberately no WriteTimeout/ReadTimeout:
		// either would silently sever a long-lived event stream once its
		// deadline elapsed, mid-stream, with no way for the handler to renew it.
		ReadHeaderTimeout: 5 * time.Second,
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

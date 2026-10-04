// Package app wires the ledger service together and runs its HTTP server
// with graceful shutdown.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jsankalp715/Idempotent-Payments-Ledger-/internal/config"
	"github.com/jsankalp715/Idempotent-Payments-Ledger-/internal/httpapi"
	"github.com/jsankalp715/Idempotent-Payments-Ledger-/internal/idempotency"
	"github.com/jsankalp715/Idempotent-Payments-Ledger-/internal/ledger"
	"github.com/jsankalp715/Idempotent-Payments-Ledger-/internal/postgres"
)

// App is a fully wired ledger service.
type App struct {
	cfg       config.Config
	logger    *slog.Logger
	pool      *pgxpool.Pool
	ownsPool  bool
	runner    *postgres.TxRunner
	store     *idempotency.Store
	health    *httpapi.Health
	handler   http.Handler
	closeOnce sync.Once
}

// New connects to the configured database, applies migrations if enabled and
// wires the services. Close releases the pool.
func New(ctx context.Context, cfg config.Config, logger *slog.Logger) (*App, error) {
	pool, err := postgres.NewPool(ctx, cfg.DatabaseURL, postgres.PoolOptions{MaxConns: cfg.DBMaxConns, MinConns: cfg.DBMinConns})
	if err != nil {
		return nil, err
	}
	if cfg.MigrateOnStart {
		if err := postgres.Migrate(ctx, pool, logger); err != nil {
			pool.Close()
			return nil, err
		}
	}
	a := NewWithPool(cfg, pool, logger)
	a.ownsPool = true
	return a, nil
}

// NewWithPool wires the services over an existing, migrated pool. The
// caller keeps ownership of the pool.
func NewWithPool(cfg config.Config, pool *pgxpool.Pool, logger *slog.Logger) *App {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	runner := postgres.NewTxRunner(pool, postgres.TxOptions{
		Isolation:   cfg.TxIsolation,
		MaxAttempts: cfg.TxMaxAttempts,
		BaseDelay:   cfg.TxRetryBaseDelay,
		MaxDelay:    cfg.TxRetryMaxDelay,
		RetryableUniqueConstraints: []string{
			idempotency.KeyConstraint,
			ledger.TransferKeyConstraint,
		},
		Logger: logger,
	})
	store := idempotency.NewStore(runner, cfg.IdempotencyTTL, logger)
	health := httpapi.NewHealth(pool)
	handler := httpapi.New(httpapi.Config{
		Ledger:         ledger.NewService(),
		Runner:         runner,
		Idempotency:    store,
		Health:         health,
		Logger:         logger,
		RequestTimeout: cfg.RequestTimeout,
	})
	return &App{cfg: cfg, logger: logger, pool: pool, runner: runner, store: store, health: health, handler: handler}
}

// Handler is the root HTTP handler.
func (a *App) Handler() http.Handler { return a.handler }

// Runner exposes the transaction runner (and its retry statistics).
func (a *App) Runner() *postgres.TxRunner { return a.runner }

// Store exposes the idempotency store.
func (a *App) Store() *idempotency.Store { return a.store }

// Close releases resources the App owns.
func (a *App) Close() {
	a.closeOnce.Do(func() {
		if a.ownsPool {
			a.pool.Close()
		}
	})
}

// Serve runs the HTTP server on ln and the idempotency-key cleanup job until
// ctx is cancelled, then shuts down gracefully:
//
//  1. /healthz starts answering 503 "draining"; the server keeps serving for
//     ShutdownDrainDelay so load balancers can take the instance out.
//  2. The listener closes and in-flight requests get up to ShutdownTimeout
//     to finish. Their transactions commit or roll back normally.
//  3. Requests still running after that have their contexts cancelled (their
//     transactions roll back) and connections are closed.
//
// Serve returns nil after a clean shutdown.
func (a *App) Serve(ctx context.Context, ln net.Listener) error {
	baseCtx, cancelBase := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelBase()
	srv := &http.Server{
		Handler:           a.handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      a.cfg.RequestTimeout + 5*time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(a.logger.Handler(), slog.LevelWarn),
		BaseContext:       func(net.Listener) context.Context { return baseCtx },
	}

	var jobs sync.WaitGroup
	jobsCtx, stopJobs := context.WithCancel(baseCtx)
	defer stopJobs()
	jobs.Add(1)
	go func() {
		defer jobs.Done()
		a.store.RunCleanup(jobsCtx, a.cfg.IdempotencyCleanupEvery, a.cfg.IdempotencyCleanupBatch)
	}()

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	a.logger.Info("ledger listening", "addr", ln.Addr().String(), "tx_isolation", string(a.cfg.TxIsolation))

	select {
	case err := <-serveErr:
		stopJobs()
		jobs.Wait()
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}

	a.logger.Info("shutdown started", "drain_delay", a.cfg.ShutdownDrainDelay, "timeout", a.cfg.ShutdownTimeout)
	a.health.SetDraining()
	if d := a.cfg.ShutdownDrainDelay; d > 0 {
		time.Sleep(d)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), a.cfg.ShutdownTimeout)
	defer cancel()
	shutdownErr := srv.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		a.logger.Warn("graceful shutdown timed out; cancelling remaining requests", "error", shutdownErr)
		cancelBase()
		_ = srv.Close()
	}
	stopJobs()
	jobs.Wait()
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http server: %w", err)
	}
	if shutdownErr != nil {
		return fmt.Errorf("shutdown: %w", shutdownErr)
	}
	a.logger.Info("shutdown complete")
	return nil
}

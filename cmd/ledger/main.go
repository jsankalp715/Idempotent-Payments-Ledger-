// Command ledger runs the idempotent payments ledger HTTP service.
//
//	ledger [serve]      run the HTTP server (default)
//	ledger migrate      apply database migrations and exit
//	ledger healthcheck  GET /healthz on the local server; exit 0 if healthy
//
// Configuration comes from environment variables; see internal/config.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jsankalp715/Idempotent-Payments-Ledger-/internal/app"
	"github.com/jsankalp715/Idempotent-Payments-Ledger-/internal/config"
	"github.com/jsankalp715/Idempotent-Payments-Ledger-/internal/postgres"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Getenv, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "ledger: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout io.Writer) error {
	cmd := "serve"
	if len(args) > 0 {
		cmd = args[0]
	}
	switch cmd {
	case "serve":
		return serve(ctx, getenv, stdout)
	case "migrate":
		return migrate(ctx, getenv, stdout)
	case "healthcheck":
		return healthcheck(ctx, getenv)
	case "help", "-h", "--help":
		_, err := fmt.Fprintln(stdout, "usage: ledger [serve|migrate|healthcheck]")
		return err
	default:
		return fmt.Errorf("unknown command %q (want serve, migrate or healthcheck)", cmd)
	}
}

func newLogger(cfg config.Config, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	if cfg.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

func serve(ctx context.Context, getenv func(string) string, stdout io.Writer) error {
	cfg, err := config.Load(getenv)
	if err != nil {
		return err
	}
	logger := newLogger(cfg, stdout)
	slog.SetDefault(logger)

	a, err := app.New(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer a.Close()

	ln, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.HTTPAddr, err)
	}
	return a.Serve(ctx, ln)
}

func migrate(ctx context.Context, getenv func(string) string, stdout io.Writer) error {
	cfg, err := config.Load(getenv)
	if err != nil {
		return err
	}
	logger := newLogger(cfg, stdout)
	pool, err := postgres.NewPool(ctx, cfg.DatabaseURL, postgres.PoolOptions{MaxConns: 2})
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := postgres.Migrate(ctx, pool, logger); err != nil {
		return err
	}
	logger.Info("migrations up to date")
	return nil
}

// healthcheck probes the local server. It needs no database configuration,
// so it works as a Docker HEALTHCHECK in an image without a shell or curl.
func healthcheck(ctx context.Context, getenv func(string) string) error {
	addr := getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("parse HTTP_ADDR %q: %w", addr, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	url := "http://" + net.JoinHostPort(host, port) + "/healthz"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("health check: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

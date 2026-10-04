// Package httpapi exposes the ledger over HTTP/JSON.
//
//	POST /v1/accounts                 create an account (optional Idempotency-Key)
//	GET  /v1/accounts/{id}            account with its balance
//	GET  /v1/accounts/{id}/entries    the account's entries, newest first
//	POST /v1/transfers                move money (Idempotency-Key required)
//	GET  /v1/transfers/{id}           a transfer with its two entries
//	GET  /healthz                     liveness/readiness, including the database
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jsankalp715/Idempotent-Payments-Ledger-/internal/idempotency"
	"github.com/jsankalp715/Idempotent-Payments-Ledger-/internal/ledger"
	"github.com/jsankalp715/Idempotent-Payments-Ledger-/internal/postgres"
)

// Config wires the HTTP layer to the services it exposes.
type Config struct {
	Ledger         *ledger.Service
	Runner         *postgres.TxRunner
	Idempotency    *idempotency.Store
	Health         *Health
	Logger         *slog.Logger
	RequestTimeout time.Duration
}

type server struct {
	mux            *http.ServeMux
	ledger         *ledger.Service
	runner         *postgres.TxRunner
	idem           *idempotency.Store
	health         *Health
	logger         *slog.Logger
	requestTimeout time.Duration
}

// New returns the service's root HTTP handler.
func New(cfg Config) http.Handler {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	s := &server{
		mux:            http.NewServeMux(),
		ledger:         cfg.Ledger,
		runner:         cfg.Runner,
		idem:           cfg.Idempotency,
		health:         cfg.Health,
		logger:         cfg.Logger,
		requestTimeout: cfg.RequestTimeout,
	}
	s.mux.HandleFunc("POST /v1/accounts", s.createAccount)
	s.mux.HandleFunc("GET /v1/accounts/{id}", s.getAccount)
	s.mux.HandleFunc("GET /v1/accounts/{id}/entries", s.listEntries)
	s.mux.HandleFunc("POST /v1/transfers", s.createTransfer)
	s.mux.HandleFunc("GET /v1/transfers/{id}", s.getTransfer)
	s.mux.HandleFunc("GET /healthz", s.healthz)
	return s.withRequestContext(http.HandlerFunc(s.route))
}

// route dispatches to the mux, turning its plain-text 404 and 405 replies
// into the JSON error envelope used everywhere else.
func (s *server) route(w http.ResponseWriter, r *http.Request) {
	if _, pattern := s.mux.Handler(r); pattern != "" {
		s.mux.ServeHTTP(w, r)
		return
	}
	probe := &headerOnlyWriter{header: http.Header{}}
	s.mux.ServeHTTP(probe, r)
	if probe.status == http.StatusMethodNotAllowed {
		w.Header().Set("Allow", probe.header.Get("Allow"))
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method "+r.Method+" is not allowed on "+r.URL.Path)
		return
	}
	writeError(w, http.StatusNotFound, "not_found", "no route for "+r.Method+" "+r.URL.Path)
}

// headerOnlyWriter captures the status and headers the mux would send.
type headerOnlyWriter struct {
	header http.Header
	status int
}

func (w *headerOnlyWriter) Header() http.Header         { return w.header }
func (w *headerOnlyWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w *headerOnlyWriter) WriteHeader(status int)      { w.status = status }

// Health tracks readiness. During shutdown it reports "draining" so load
// balancers stop sending traffic before the listener closes.
type Health struct {
	pool     *pgxpool.Pool
	draining atomic.Bool
}

// NewHealth returns a Health that checks pool connectivity.
func NewHealth(pool *pgxpool.Pool) *Health { return &Health{pool: pool} }

// SetDraining makes /healthz report 503 from now on.
func (h *Health) SetDraining() { h.draining.Store(true) }

func (s *server) healthz(w http.ResponseWriter, r *http.Request) {
	if s.health.draining.Load() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "draining"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.health.pool.Ping(ctx); err != nil {
		requestLogger(r.Context(), s.logger).WarnContext(r.Context(), "health check: database unreachable", "error", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "database": "unreachable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "database": "ok"})
}

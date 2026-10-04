package postgres

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SQLSTATE codes that mean "this transaction was rolled back because of a
// conflict with a concurrent one; running it again may succeed".
const (
	CodeSerializationFailure = "40001"
	CodeDeadlockDetected     = "40P01"
	CodeUniqueViolation      = "23505"
	CodeCheckViolation       = "23514"
)

// Retry reasons, used as labels in RetryStats and logs.
const (
	ReasonSerialization = "serialization_failure"
	ReasonDeadlock      = "deadlock_detected"
	ReasonUniqueRace    = "unique_race"
	ReasonConnection    = "connection"
)

// ErrRetriesExhausted is wrapped by the error returned when a transaction
// still conflicted after the last allowed attempt. Nothing was committed, so
// the caller may safely try again later.
var ErrRetriesExhausted = errors.New("transaction retries exhausted")

// TxOptions configures a TxRunner.
type TxOptions struct {
	Isolation pgx.TxIsoLevel
	// MaxAttempts bounds the number of times a transaction is run (>= 1).
	MaxAttempts int
	// BaseDelay and MaxDelay shape the exponential backoff between attempts:
	// the sleep before retry n is uniformly random in
	// [0, min(MaxDelay, BaseDelay*2^(n-1))] ("full jitter").
	BaseDelay time.Duration
	MaxDelay  time.Duration
	// RetryableUniqueConstraints names unique constraints whose violation
	// means a concurrent transaction committed the same logical write first.
	// Retrying lets the next attempt observe the winner and react to it.
	RetryableUniqueConstraints []string
	Logger                     *slog.Logger
}

// TxRunner runs functions inside database transactions and transparently
// retries the whole transaction on serialization failures, deadlocks and
// configured unique-constraint races. Retries are bounded; each attempt is a
// brand-new transaction, so the function must not keep state across attempts.
type TxRunner struct {
	pool      *pgxpool.Pool
	opts      TxOptions
	retryable map[string]bool
	stats     retryCounters
}

// NewTxRunner returns a TxRunner over pool.
func NewTxRunner(pool *pgxpool.Pool, opts TxOptions) *TxRunner {
	if opts.MaxAttempts < 1 {
		opts.MaxAttempts = 1
	}
	if opts.BaseDelay <= 0 {
		opts.BaseDelay = time.Millisecond
	}
	if opts.MaxDelay < opts.BaseDelay {
		opts.MaxDelay = opts.BaseDelay
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	retryable := make(map[string]bool, len(opts.RetryableUniqueConstraints))
	for _, c := range opts.RetryableUniqueConstraints {
		retryable[c] = true
	}
	return &TxRunner{pool: pool, opts: opts, retryable: retryable}
}

// Pool returns the underlying pool, for reads that need no transaction.
func (r *TxRunner) Pool() *pgxpool.Pool { return r.pool }

// Run executes fn in a transaction, committing if fn returns nil and rolling
// back otherwise. Retryable failures (from fn or from COMMIT) restart the
// transaction after a backoff, up to MaxAttempts runs in total.
func (r *TxRunner) Run(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	for attempt := 1; ; attempt++ {
		err := r.runOnce(ctx, fn)
		if err == nil {
			return nil
		}
		reason, ok := r.classify(err)
		if !ok {
			return err
		}
		r.stats.record(reason)
		if attempt >= r.opts.MaxAttempts {
			r.stats.exhausted.Add(1)
			r.opts.Logger.WarnContext(ctx, "transaction retries exhausted", "attempts", attempt, "reason", reason, "error", err)
			return fmt.Errorf("%w after %d attempts: %w", ErrRetriesExhausted, attempt, err)
		}
		delay := r.backoff(attempt)
		r.opts.Logger.DebugContext(ctx, "retrying transaction", "attempt", attempt, "reason", reason, "delay", delay.String())
		t := time.NewTimer(delay)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("transaction retry abandoned: %w (last error: %w)", ctx.Err(), err)
		}
	}
}

// InTx is Run for functions that produce a value.
func InTx[T any](ctx context.Context, r *TxRunner, fn func(ctx context.Context, tx pgx.Tx) (T, error)) (T, error) {
	var out T
	err := r.Run(ctx, func(ctx context.Context, tx pgx.Tx) error {
		v, err := fn(ctx, tx)
		if err != nil {
			return err
		}
		out = v
		return nil
	})
	return out, err
}

func (r *TxRunner) runOnce(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: r.opts.Isolation})
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	// Rolling back after a successful commit is a no-op. A detached context
	// lets the rollback reach the server even when ctx was cancelled, so the
	// connection goes back to the pool instead of being closed.
	defer func() {
		rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(rbCtx)
	}()
	if err := fn(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// classify reports whether err is a transient conflict worth retrying. Only
// errors that guarantee nothing was committed qualify: a PostgreSQL error
// aborts the whole transaction, and SafeToRetry errors happened before any
// bytes reached the server. A broken connection during COMMIT is ambiguous,
// so it is returned to the caller (idempotency keys make the client's retry
// safe) instead of being retried here.
func (r *TxRunner) classify(err error) (string, bool) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case CodeSerializationFailure:
			return ReasonSerialization, true
		case CodeDeadlockDetected:
			return ReasonDeadlock, true
		case CodeUniqueViolation:
			if r.retryable[pgErr.ConstraintName] {
				return ReasonUniqueRace, true
			}
		}
		return "", false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "", false
	}
	if pgconn.SafeToRetry(err) {
		return ReasonConnection, true
	}
	return "", false
}

func (r *TxRunner) backoff(attempt int) time.Duration {
	ceiling := r.opts.MaxDelay
	if shift := attempt - 1; shift < 30 {
		if d := r.opts.BaseDelay << shift; d > 0 && d < ceiling {
			ceiling = d
		}
	}
	return time.Duration(rand.Int64N(int64(ceiling) + 1))
}

// RetryStats counts retried attempts by reason since the runner was created.
type RetryStats struct {
	Serialization int64
	Deadlock      int64
	UniqueRace    int64
	Connection    int64
	// Exhausted counts transactions that gave up after MaxAttempts.
	Exhausted int64
}

// Total returns the number of retried attempts across all reasons.
func (s RetryStats) Total() int64 {
	return s.Serialization + s.Deadlock + s.UniqueRace + s.Connection
}

// Stats returns a snapshot of the retry counters.
func (r *TxRunner) Stats() RetryStats {
	return RetryStats{
		Serialization: r.stats.serialization.Load(),
		Deadlock:      r.stats.deadlock.Load(),
		UniqueRace:    r.stats.uniqueRace.Load(),
		Connection:    r.stats.connection.Load(),
		Exhausted:     r.stats.exhausted.Load(),
	}
}

type retryCounters struct {
	serialization, deadlock, uniqueRace, connection, exhausted atomic.Int64
}

func (c *retryCounters) record(reason string) {
	switch reason {
	case ReasonSerialization:
		c.serialization.Add(1)
	case ReasonDeadlock:
		c.deadlock.Add(1)
	case ReasonUniqueRace:
		c.uniqueRace.Add(1)
	case ReasonConnection:
		c.connection.Add(1)
	}
}

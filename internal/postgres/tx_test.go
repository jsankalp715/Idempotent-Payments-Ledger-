package postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestClassify(t *testing.T) {
	r := NewTxRunner(nil, TxOptions{RetryableUniqueConstraints: []string{"idempotency_keys_pkey"}})
	cases := []struct {
		name      string
		err       error
		reason    string
		retryable bool
	}{
		{"serialization failure", &pgconn.PgError{Code: "40001"}, ReasonSerialization, true},
		{"deadlock", &pgconn.PgError{Code: "40P01"}, ReasonDeadlock, true},
		{"wrapped deadlock", fmt.Errorf("commit: %w", &pgconn.PgError{Code: "40P01"}), ReasonDeadlock, true},
		{"unique race on listed constraint", &pgconn.PgError{Code: "23505", ConstraintName: "idempotency_keys_pkey"}, ReasonUniqueRace, true},
		{"unique violation elsewhere", &pgconn.PgError{Code: "23505", ConstraintName: "other_key"}, "", false},
		{"check violation", &pgconn.PgError{Code: "23514"}, "", false},
		{"lock timeout", &pgconn.PgError{Code: "55P03"}, "", false},
		{"context canceled", context.Canceled, "", false},
		{"deadline", fmt.Errorf("query: %w", context.DeadlineExceeded), "", false},
		{"plain error", errors.New("boom"), "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reason, ok := r.classify(c.err)
			if ok != c.retryable || reason != c.reason {
				t.Fatalf("classify(%v) = (%q, %v), want (%q, %v)", c.err, reason, ok, c.reason, c.retryable)
			}
		})
	}
}

func TestBackoffIsBoundedAndJittered(t *testing.T) {
	r := NewTxRunner(nil, TxOptions{BaseDelay: 4 * time.Millisecond, MaxDelay: 50 * time.Millisecond})
	for attempt := 1; attempt <= 40; attempt++ {
		ceiling := 4 * time.Millisecond << (attempt - 1)
		if attempt > 10 || ceiling > 50*time.Millisecond {
			ceiling = 50 * time.Millisecond
		}
		seen := map[time.Duration]bool{}
		for i := 0; i < 200; i++ {
			d := r.backoff(attempt)
			if d < 0 || d > ceiling {
				t.Fatalf("attempt %d: backoff %v outside [0, %v]", attempt, d, ceiling)
			}
			seen[d] = true
		}
		if len(seen) < 10 {
			t.Fatalf("attempt %d: backoff is not jittered (%d distinct values)", attempt, len(seen))
		}
	}
}

func TestNewTxRunnerNormalizesOptions(t *testing.T) {
	r := NewTxRunner(nil, TxOptions{MaxAttempts: 0, BaseDelay: 0, MaxDelay: 0})
	if r.opts.MaxAttempts != 1 || r.opts.BaseDelay <= 0 || r.opts.MaxDelay < r.opts.BaseDelay || r.opts.Logger == nil {
		t.Fatalf("options not normalized: %+v", r.opts)
	}
	if r.opts.Isolation != pgx.TxIsoLevel("") {
		t.Fatalf("unexpected isolation %q", r.opts.Isolation)
	}
}

func TestRetryStatsTotal(t *testing.T) {
	s := RetryStats{Serialization: 1, Deadlock: 2, UniqueRace: 3, Connection: 4, Exhausted: 100}
	if s.Total() != 10 {
		t.Fatalf("Total() = %d, want 10", s.Total())
	}
}

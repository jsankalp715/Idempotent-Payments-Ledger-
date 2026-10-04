package postgres_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jsankalp715/Idempotent-Payments-Ledger-/internal/postgres"
	"github.com/jsankalp715/Idempotent-Payments-Ledger-/internal/testdb"
)

func raise(ctx context.Context, tx pgx.Tx, errcode string) error {
	_, err := tx.Exec(ctx, `DO $$ BEGIN RAISE EXCEPTION 'injected' USING ERRCODE = '`+errcode+`'; END $$`)
	return err
}

func newRunner(db *testdb.DB, attempts int) *postgres.TxRunner {
	return postgres.NewTxRunner(db.Pool, postgres.TxOptions{
		Isolation:                  pgx.ReadCommitted,
		MaxAttempts:                attempts,
		BaseDelay:                  time.Millisecond,
		MaxDelay:                   5 * time.Millisecond,
		RetryableUniqueConstraints: []string{"demo_pkey"},
	})
}

func TestTxRunnerRetriesTransientFailuresAndCommitsOnce(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	if _, err := db.Pool.Exec(ctx, `CREATE TABLE demo (id int PRIMARY KEY, attempt int NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	r := newRunner(db, 5)

	var calls atomic.Int32
	err := r.Run(ctx, func(ctx context.Context, tx pgx.Tx) error {
		n := calls.Add(1)
		// Each attempt writes first, so a rollback must discard the write.
		if _, err := tx.Exec(ctx, `INSERT INTO demo (id, attempt) VALUES (1, $1)`, n); err != nil {
			return err
		}
		switch n {
		case 1:
			return raise(ctx, tx, "serialization_failure")
		case 2:
			return raise(ctx, tx, "deadlock_detected")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("fn ran %d times, want 3", calls.Load())
	}
	var rows, attempt int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*), max(attempt) FROM demo`).Scan(&rows, &attempt); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || attempt != 3 {
		t.Fatalf("demo has %d rows (attempt %d), want exactly the third attempt's row", rows, attempt)
	}
	s := r.Stats()
	if s.Serialization != 1 || s.Deadlock != 1 || s.Exhausted != 0 {
		t.Fatalf("unexpected stats %+v", s)
	}
}

func TestTxRunnerGivesUpAfterMaxAttempts(t *testing.T) {
	db := testdb.New(t)
	r := newRunner(db, 4)
	var calls atomic.Int32
	err := r.Run(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		calls.Add(1)
		return raise(ctx, tx, "serialization_failure")
	})
	if !errors.Is(err, postgres.ErrRetriesExhausted) {
		t.Fatalf("err = %v, want ErrRetriesExhausted", err)
	}
	if calls.Load() != 4 {
		t.Fatalf("fn ran %d times, want 4", calls.Load())
	}
	if s := r.Stats(); s.Serialization != 4 || s.Exhausted != 1 {
		t.Fatalf("unexpected stats %+v", s)
	}
}

func TestTxRunnerDoesNotRetryPermanentErrors(t *testing.T) {
	db := testdb.New(t)
	r := newRunner(db, 5)
	var calls atomic.Int32
	err := r.Run(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		calls.Add(1)
		return raise(ctx, tx, "check_violation")
	})
	if err == nil || errors.Is(err, postgres.ErrRetriesExhausted) {
		t.Fatalf("err = %v, want the check violation itself", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("fn ran %d times, want 1", calls.Load())
	}
}

func TestTxRunnerRetriesListedUniqueRaces(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	if _, err := db.Pool.Exec(ctx, `CREATE TABLE demo (id int PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO demo VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	r := newRunner(db, 5)
	var calls atomic.Int32
	err := r.Run(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// First attempt collides with the committed row, like a request that
		// lost a race; the retry sees the winner and does nothing.
		if calls.Add(1) == 1 {
			_, err := tx.Exec(ctx, `INSERT INTO demo VALUES (1)`)
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls.Load() != 2 || r.Stats().UniqueRace != 1 {
		t.Fatalf("calls=%d stats=%+v", calls.Load(), r.Stats())
	}
}

func TestTxRunnerStopsRetryingWhenContextEnds(t *testing.T) {
	db := testdb.New(t)
	r := postgres.NewTxRunner(db.Pool, postgres.TxOptions{MaxAttempts: 1000, BaseDelay: 50 * time.Millisecond, MaxDelay: 50 * time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := r.Run(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return raise(ctx, tx, "serialization_failure")
	})
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Run kept going for %v after the context ended", elapsed)
	}
}

// TestTxRunnerRecoversFromARealDeadlock provokes a genuine deadlock (two
// transactions locking two rows in opposite order) and checks that
// PostgreSQL's deadlock detector aborts one of them and the runner retries it
// to completion.
func TestTxRunnerRecoversFromARealDeadlock(t *testing.T) {
	db := testdb.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := db.Pool.Exec(ctx, `CREATE TABLE demo (id int PRIMARY KEY, v int NOT NULL); INSERT INTO demo VALUES (1, 0), (2, 0)`); err != nil {
		t.Fatal(err)
	}
	r := newRunner(db, 5)

	var both sync.WaitGroup
	both.Add(2)
	var once [2]sync.Once
	worker := func(i, first, second int) error {
		return r.Run(ctx, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `UPDATE demo SET v = v + 1 WHERE id = $1`, first); err != nil {
				return err
			}
			// On the first attempt only, wait until both transactions hold
			// their first lock, guaranteeing a lock cycle.
			once[i].Do(func() { both.Done(); both.Wait() })
			_, err := tx.Exec(ctx, `UPDATE demo SET v = v + 1 WHERE id = $1`, second)
			return err
		})
	}
	errs := make(chan error, 2)
	go func() { errs <- worker(0, 1, 2) }()
	go func() { errs <- worker(1, 2, 1) }()
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("worker failed: %v", err)
		}
	}
	if s := r.Stats(); s.Deadlock < 1 {
		t.Fatalf("expected at least one deadlock retry, stats %+v", s)
	}
	var v1, v2 int
	if err := db.Pool.QueryRow(ctx, `SELECT (SELECT v FROM demo WHERE id = 1), (SELECT v FROM demo WHERE id = 2)`).Scan(&v1, &v2); err != nil {
		t.Fatal(err)
	}
	if v1 != 2 || v2 != 2 {
		t.Fatalf("v1=%d v2=%d, want 2 and 2 (each transaction applied exactly once)", v1, v2)
	}
}

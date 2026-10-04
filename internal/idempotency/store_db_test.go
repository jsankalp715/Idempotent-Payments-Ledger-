package idempotency_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jsankalp715/Idempotent-Payments-Ledger-/internal/idempotency"
	"github.com/jsankalp715/Idempotent-Payments-Ledger-/internal/postgres"
	"github.com/jsankalp715/Idempotent-Payments-Ledger-/internal/testdb"
)

type harness struct {
	db    *testdb.DB
	store *idempotency.Store
	calls atomic.Int32
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	db := testdb.New(t, testdb.Options{MaxConns: 20})
	if _, err := db.Pool.Exec(context.Background(), `CREATE TABLE work_log (key text NOT NULL, n int NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	runner := postgres.NewTxRunner(db.Pool, postgres.TxOptions{
		MaxAttempts:                5,
		BaseDelay:                  time.Millisecond,
		MaxDelay:                   10 * time.Millisecond,
		RetryableUniqueConstraints: []string{idempotency.KeyConstraint},
	})
	return &harness{db: db, store: idempotency.NewStore(runner, time.Hour, nil)}
}

func request(key, body string) idempotency.Request {
	h := sha256.Sum256([]byte(body))
	return idempotency.Request{Key: key, Method: "POST", Path: "/v1/things", Hash: h[:]}
}

// work records each execution in work_log (inside the transaction) and
// returns a response that identifies the execution.
func (h *harness) work(key string) idempotency.Work {
	return func(ctx context.Context, tx pgx.Tx) (idempotency.Response, error) {
		n := h.calls.Add(1)
		if _, err := tx.Exec(ctx, `INSERT INTO work_log (key, n) VALUES ($1, $2)`, key, n); err != nil {
			return idempotency.Response{}, err
		}
		return idempotency.Response{Status: 201, Body: []byte(fmt.Sprintf(`{"execution":%d}`, n))}, nil
	}
}

func (h *harness) workLogCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := h.db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM work_log`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (h *harness) keyCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := h.db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM idempotency_keys`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestDoExecutesOnceAndReplaysTheStoredResponse(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	req := request("k1", "body")

	first, replayed, err := h.store.Do(ctx, req, h.work("k1"))
	if err != nil || replayed {
		t.Fatalf("first Do: replayed=%v err=%v", replayed, err)
	}
	for i := 0; i < 3; i++ {
		again, replayed, err := h.store.Do(ctx, req, h.work("k1"))
		if err != nil || !replayed {
			t.Fatalf("repeat Do: replayed=%v err=%v", replayed, err)
		}
		if again.Status != first.Status || !bytes.Equal(again.Body, first.Body) {
			t.Fatalf("replayed %d %s, want %d %s", again.Status, again.Body, first.Status, first.Body)
		}
	}
	if h.calls.Load() != 1 || h.workLogCount(t) != 1 {
		t.Fatalf("work ran %d times (%d committed), want exactly once", h.calls.Load(), h.workLogCount(t))
	}
}

func TestDoRejectsKeyReuseWithADifferentRequest(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, _, err := h.store.Do(ctx, request("k1", "body-a"), h.work("k1")); err != nil {
		t.Fatal(err)
	}
	_, _, err := h.store.Do(ctx, request("k1", "body-b"), h.work("k1"))
	if !errors.Is(err, idempotency.ErrKeyReused) {
		t.Fatalf("err = %v, want ErrKeyReused", err)
	}
	if h.calls.Load() != 1 {
		t.Fatalf("work ran %d times, want 1", h.calls.Load())
	}
}

func TestDoReportsInFlightWhileAnotherTransactionHoldsTheKey(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	conn := h.db.Conn(t)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, idempotency.LockID("k1")); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	_, _, err = h.store.Do(ctx, request("k1", "body"), h.work("k1"))
	if !errors.Is(err, idempotency.ErrInFlight) {
		t.Fatalf("err = %v, want ErrInFlight", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("Do waited %v; it must fail fast instead of queueing", time.Since(start))
	}
	if h.calls.Load() != 0 {
		t.Fatal("work ran while the key was in flight")
	}

	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if _, replayed, err := h.store.Do(ctx, request("k1", "body"), h.work("k1")); err != nil || replayed {
		t.Fatalf("Do after release: replayed=%v err=%v", replayed, err)
	}
}

func TestDoStoresNothingWhenWorkFails(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	boom := errors.New("transient failure")
	_, _, err := h.store.Do(ctx, request("k1", "body"), func(ctx context.Context, tx pgx.Tx) (idempotency.Response, error) {
		if _, err := tx.Exec(ctx, `INSERT INTO work_log (key, n) VALUES ('k1', 0)`); err != nil {
			return idempotency.Response{}, err
		}
		return idempotency.Response{}, boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if h.workLogCount(t) != 0 || h.keyCount(t) != 0 {
		t.Fatalf("failed work left %d work rows and %d keys behind", h.workLogCount(t), h.keyCount(t))
	}
	// The same key is still usable: the failure was not cached.
	if _, replayed, err := h.store.Do(ctx, request("k1", "body"), h.work("k1")); err != nil || replayed {
		t.Fatalf("retry: replayed=%v err=%v", replayed, err)
	}
	if h.workLogCount(t) != 1 || h.keyCount(t) != 1 {
		t.Fatalf("retry left %d work rows and %d keys, want 1 and 1", h.workLogCount(t), h.keyCount(t))
	}
}

func TestDoCommitsTheRecordAndTheWorkAtomically(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	// Make storing the key fail after the work has written its row.
	if _, err := h.db.Pool.Exec(ctx, `
		CREATE FUNCTION fail_key_insert() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'injected failure while storing the key'; END $$;
		CREATE TRIGGER fail_key_insert BEFORE INSERT ON idempotency_keys
			FOR EACH ROW EXECUTE FUNCTION fail_key_insert();`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.store.Do(ctx, request("k1", "body"), h.work("k1")); err == nil {
		t.Fatal("expected the injected failure")
	}
	if h.calls.Load() != 1 {
		t.Fatalf("work ran %d times, want 1", h.calls.Load())
	}
	if h.workLogCount(t) != 0 {
		t.Fatal("the work committed although its key record did not")
	}
	if _, err := h.db.Pool.Exec(ctx, `DROP TRIGGER fail_key_insert ON idempotency_keys`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.store.Do(ctx, request("k1", "body"), h.work("k1")); err != nil {
		t.Fatal(err)
	}
	if h.workLogCount(t) != 1 || h.keyCount(t) != 1 {
		t.Fatalf("after recovery: %d work rows, %d keys; want 1 and 1", h.workLogCount(t), h.keyCount(t))
	}
}

func TestDoTreatsExpiredRecordsAsAbsent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, _, err := h.store.Do(ctx, request("k1", "body"), h.work("k1")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Pool.Exec(ctx,
		`UPDATE idempotency_keys SET created_at = now() - interval '2 hours', expires_at = now() - interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	// Even a different request may use an expired key.
	resp, replayed, err := h.store.Do(ctx, request("k1", "other body"), h.work("k1"))
	if err != nil || replayed {
		t.Fatalf("Do on expired key: replayed=%v err=%v", replayed, err)
	}
	if string(resp.Body) != `{"execution":2}` {
		t.Fatalf("body %s, want the second execution", resp.Body)
	}
	var live bool
	if err := h.db.Pool.QueryRow(ctx, `SELECT expires_at > now() FROM idempotency_keys WHERE key = 'k1'`).Scan(&live); err != nil || !live {
		t.Fatalf("record not refreshed: live=%v err=%v", live, err)
	}
}

func TestDeleteExpiredRemovesOnlyExpiredRecordsInBatches(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.db.Pool.Exec(ctx, `
		INSERT INTO idempotency_keys (key, request_method, request_path, request_hash, response_status, response_body, created_at, expires_at)
		SELECT 'expired-' || g, 'POST', '/x', decode(repeat('00', 32), 'hex'), 201, '{}'::bytea, now() - interval '2 days', now() - interval '1 day'
		  FROM generate_series(1, 25) g
		UNION ALL
		SELECT 'live-' || g, 'POST', '/x', decode(repeat('00', 32), 'hex'), 201, '{}'::bytea, now(), now() + interval '1 day'
		  FROM generate_series(1, 5) g`); err != nil {
		t.Fatal(err)
	}
	n, err := h.store.DeleteExpired(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if n != 25 {
		t.Fatalf("deleted %d, want 25", n)
	}
	var left int
	if err := h.db.Pool.QueryRow(ctx, `SELECT count(*) FROM idempotency_keys WHERE key LIKE 'live-%'`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 5 || h.keyCount(t) != 5 {
		t.Fatalf("%d live keys and %d total remain, want 5 and 5", left, h.keyCount(t))
	}
}

func TestRunCleanupRunsUntilCancelled(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := h.db.Pool.Exec(ctx, `
		INSERT INTO idempotency_keys (key, request_method, request_path, request_hash, response_status, response_body, created_at, expires_at)
		VALUES ('old', 'POST', '/x', decode(repeat('00', 32), 'hex'), 201, '{}', now() - interval '2 days', now() - interval '1 day')`); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		h.store.RunCleanup(ctx, 10*time.Millisecond, 100)
		close(done)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for h.keyCount(t) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("cleanup did not delete the expired key")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunCleanup did not return after cancellation")
	}
}

func TestConcurrentDoWithTheSameKeyExecutesOnce(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	req := request("hot-key", "body")
	slowWork := func(ctx context.Context, tx pgx.Tx) (idempotency.Response, error) {
		time.Sleep(20 * time.Millisecond) // widen the in-flight window
		return h.work("hot-key")(ctx, tx)
	}

	const n = 40
	var wg sync.WaitGroup
	start := make(chan struct{})
	var mu sync.Mutex
	var bodies [][]byte
	var inFlight int
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			resp, _, err := h.store.Do(ctx, req, slowWork)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case errors.Is(err, idempotency.ErrInFlight):
				inFlight++
			case err != nil:
				t.Errorf("Do: %v", err)
			default:
				bodies = append(bodies, resp.Body)
			}
		}()
	}
	close(start)
	wg.Wait()

	if h.calls.Load() != 1 || h.workLogCount(t) != 1 {
		t.Fatalf("work ran %d times (%d committed), want exactly once", h.calls.Load(), h.workLogCount(t))
	}
	if inFlight == 0 {
		t.Fatal("expected some requests to observe the key in flight")
	}
	for _, b := range bodies {
		if !bytes.Equal(b, bodies[0]) {
			t.Fatalf("responses differ: %s vs %s", b, bodies[0])
		}
	}
	t.Logf("%d executed or replayed, %d saw 409 in flight", len(bodies), inFlight)
}

func TestDoValidatesItsInput(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, _, err := h.store.Do(ctx, request("", "body"), h.work("")); !errors.Is(err, idempotency.ErrInvalidKey) {
		t.Fatalf("empty key: err = %v", err)
	}
	bad := request("k", "body")
	bad.Hash = bad.Hash[:5]
	if _, _, err := h.store.Do(ctx, bad, h.work("k")); err == nil {
		t.Fatal("short hash accepted")
	}
	if h.calls.Load() != 0 {
		t.Fatal("work ran for invalid input")
	}
}

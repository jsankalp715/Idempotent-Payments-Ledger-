package integration_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"testing"
	"time"

	"github.com/jsankalp715/Idempotent-Payments-Ledger-/internal/idempotency"
	"github.com/jsankalp715/Idempotent-Payments-Ledger-/internal/ledgertest"
	"github.com/jsankalp715/Idempotent-Payments-Ledger-/internal/postgres"
)

// newKey returns a random idempotency key, unique across parallel test runs
// sharing one database (advisory locks are database-wide).
func newKey(t *testing.T) string {
	t.Helper()
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "it-" + hex.EncodeToString(b)
}

func requireInvariants(t *testing.T, s *ledgertest.Server) ledgertest.Summary {
	t.Helper()
	sum, err := ledgertest.CheckInvariants(context.Background(), s.DB.Pool)
	if err != nil {
		t.Fatalf("ledger invariants violated:\n%v", err)
	}
	return sum
}

func count(t *testing.T, s *ledgertest.Server, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.DB.Pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func exec(t *testing.T, s *ledgertest.Server, sql string, args ...any) {
	t.Helper()
	if _, err := s.DB.Pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// waitForKeyLock blocks until some transaction holds the advisory lock of
// key, i.e. a request with that key is in flight.
func waitForKeyLock(t *testing.T, db postgres.DBTX, key string) {
	t.Helper()
	id := uint64(idempotency.LockID(key))
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		err := db.QueryRow(context.Background(), `SELECT count(*) FROM pg_locks
		                   WHERE locktype = 'advisory' AND granted AND objsubid = 1
		                     AND classid = $1::bigint::oid AND objid = $2::bigint::oid
		                     AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`,
			int64(id>>32), int64(id&0xffffffff)).Scan(&n)
		if err != nil {
			t.Fatalf("query pg_locks: %v", err)
		}
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no transaction took the lock for key %s", key)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// transferUntilDefinitive behaves like a well-written client: it retries
// with the same key on 409 (in flight), 503 (transient) and transport errors
// until it gets a definitive answer (201 or 422).
func transferUntilDefinitive(t *testing.T, c *ledgertest.Client, key string, req ledgertest.TransferRequest) ledgertest.Response {
	t.Helper()
	for attempt := 1; attempt <= 100; attempt++ {
		resp, err := c.Transfer(context.Background(), key, req)
		if err == nil && (resp.Status == 201 || resp.Status == 422) {
			return resp
		}
		if err == nil && resp.Status != 409 && resp.Status != 503 {
			t.Errorf("unexpected response %s", resp)
			return resp
		}
		time.Sleep(time.Duration(attempt) * 2 * time.Millisecond)
	}
	t.Errorf("no definitive response for key %s after 100 attempts", key)
	return ledgertest.Response{}
}

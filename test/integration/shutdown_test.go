package integration_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/jsankalp715/Idempotent-Payments-Ledger-/internal/app"
	"github.com/jsankalp715/Idempotent-Payments-Ledger-/internal/config"
	"github.com/jsankalp715/Idempotent-Payments-Ledger-/internal/ledgertest"
	"github.com/jsankalp715/Idempotent-Payments-Ledger-/internal/testdb"
)

type runningApp struct {
	addr     string
	db       *testdb.DB
	client   *ledgertest.Client
	stop     context.CancelFunc
	done     chan struct{} // closed when Serve returns
	serveErr error         // Serve's result, valid once done is closed
}

func startApp(t *testing.T, configure func(*config.Config)) *runningApp {
	t.Helper()
	db := testdb.New(t)
	cfg := ledgertest.TestConfig(t)
	configure(&cfg)
	a := app.NewWithPool(cfg, db.Pool, nil)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	r := &runningApp{
		addr:   ln.Addr().String(),
		db:     db,
		client: ledgertest.NewClient("http://"+ln.Addr().String(), &http.Client{Timeout: 30 * time.Second}),
		stop:   stop,
		done:   make(chan struct{}),
	}
	go func() {
		r.serveErr = a.Serve(ctx, ln)
		close(r.done)
	}()
	t.Cleanup(func() {
		stop()
		select {
		case <-r.done:
		case <-time.After(30 * time.Second):
			t.Error("server did not stop")
		}
	})
	return r
}

// TestGracefulShutdownFinishesInFlightTransfers starts a transfer that blocks
// on a row lock and triggers shutdown. It checks each phase: /healthz reports
// draining while the server still serves, then the listener closes while the
// transfer is still running, and the transfer still commits once its lock is
// released, after which Serve returns cleanly.
func TestGracefulShutdownFinishesInFlightTransfers(t *testing.T) {
	r := startApp(t, func(c *config.Config) {
		c.ShutdownDrainDelay = time.Second
		c.ShutdownTimeout = 30 * time.Second
	})
	ctx := context.Background()
	from := r.client.CreateAccount(t, "USD", 1_000)
	to := r.client.CreateAccount(t, "USD", 0)

	blocker := r.db.Conn(t)
	tx, err := blocker.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM accounts WHERE id = $1 FOR UPDATE`, from.ID); err != nil {
		t.Fatal(err)
	}
	key := newKey(t)
	inFlight := make(chan ledgertest.Response, 1)
	go func() {
		resp, err := r.client.Transfer(ctx, key, ledgertest.TransferRequest{FromAccount: from.ID, ToAccount: to.ID, Amount: 400, Currency: "USD"})
		if err != nil {
			t.Errorf("in-flight transfer: %v", err)
		}
		inFlight <- resp
	}()
	waitForKeyLock(t, r.db.Pool, key)

	r.stop()

	// Phase 1: during the drain delay the server still answers, but reports draining.
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := r.client.Do(ctx, http.MethodGet, "/healthz", nil)
		if err == nil && resp.Status == http.StatusServiceUnavailable {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("healthz never reported draining (last: %v %v)", resp, err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Phase 2: the listener closes while the transfer is still blocked, so
	// Shutdown is now waiting for it.
	deadline = time.Now().Add(10 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", r.addr, time.Second)
		if err != nil {
			break
		}
		_ = conn.Close()
		if time.Now().After(deadline) {
			t.Fatal("listener still accepting connections after shutdown started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case resp := <-inFlight:
		t.Fatalf("in-flight transfer finished before its lock was released: %s", resp)
	case <-r.done:
		t.Fatalf("Serve returned (%v) while a request was still in flight", r.serveErr)
	default:
	}

	// Phase 3: release the lock; the request completes and Serve returns.
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if resp := <-inFlight; resp.Status != http.StatusCreated {
		t.Fatalf("in-flight transfer: %s, want 201", resp)
	}
	select {
	case <-r.done:
		if r.serveErr != nil {
			t.Fatalf("Serve: %v", r.serveErr)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Serve did not return after the last request finished")
	}
	var n int
	if err := r.db.Pool.QueryRow(ctx, `SELECT count(*) FROM transfers WHERE idempotency_key = $1`, key).Scan(&n); err != nil || n != 1 {
		t.Fatalf("transfer committed %d times (err %v), want 1", n, err)
	}
}

// TestShutdownTimeoutCancelsStuckRequests: a request that outlives the
// shutdown timeout is cancelled; its transaction rolls back, so nothing is
// half-applied and the client can retry with the same key elsewhere.
func TestShutdownTimeoutCancelsStuckRequests(t *testing.T) {
	r := startApp(t, func(c *config.Config) {
		c.ShutdownTimeout = 300 * time.Millisecond
	})
	ctx := context.Background()
	from := r.client.CreateAccount(t, "USD", 1_000)
	to := r.client.CreateAccount(t, "USD", 0)

	blocker := r.db.Conn(t)
	tx, err := blocker.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM accounts WHERE id = $1 FOR UPDATE`, from.ID); err != nil {
		t.Fatal(err)
	}
	key := newKey(t)
	req := ledgertest.TransferRequest{FromAccount: from.ID, ToAccount: to.ID, Amount: 400, Currency: "USD"}
	go func() { _, _ = r.client.Transfer(ctx, key, req) }()
	waitForKeyLock(t, r.db.Pool, key)

	r.stop()
	select {
	case <-r.done:
		if !errors.Is(r.serveErr, context.DeadlineExceeded) {
			t.Fatalf("Serve returned %v, want a shutdown timeout", r.serveErr)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Serve did not give up after the shutdown timeout")
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	// The cancelled request committed nothing and stored no key, even after
	// the lock it was waiting for became free.
	time.Sleep(200 * time.Millisecond)
	var transfers, keys int
	if err := r.db.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM transfers WHERE idempotency_key = $1),
	                                         (SELECT count(*) FROM idempotency_keys WHERE key = $1)`, key).Scan(&transfers, &keys); err != nil {
		t.Fatal(err)
	}
	if transfers != 0 || keys != 0 {
		t.Fatalf("cancelled request left %d transfers and %d keys", transfers, keys)
	}
	var balance int64
	if err := r.db.Pool.QueryRow(ctx, `SELECT balance FROM accounts WHERE id = $1`, from.ID).Scan(&balance); err != nil || balance != 1_000 {
		t.Fatalf("source balance %d (err %v), want 1000", balance, err)
	}
}

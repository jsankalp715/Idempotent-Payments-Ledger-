package integration_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/jsankalp715/Idempotent-Payments-Ledger-/internal/ledgertest"
)

type pair struct {
	s        *ledgertest.Server
	from, to ledgertest.Account
}

// newPair starts a server with two USD accounts; from holds 10,000.
func newPair(t *testing.T) pair {
	t.Helper()
	s := ledgertest.NewServer(t)
	return pair{
		s:    s,
		from: s.Client.CreateAccount(t, "USD", 10_000),
		to:   s.Client.CreateAccount(t, "USD", 0),
	}
}

func (p pair) req(amount int64) ledgertest.TransferRequest {
	return ledgertest.TransferRequest{FromAccount: p.from.ID, ToAccount: p.to.ID, Amount: amount, Currency: "USD"}
}

func TestSameKeySamePayloadReplaysTheOriginalResponse(t *testing.T) {
	p := newPair(t)
	key := newKey(t)

	first := p.s.Client.MustTransfer(t, key, p.req(2_500))
	if first.Status != http.StatusCreated {
		t.Fatalf("first request: %s", first)
	}
	if first.Header.Get("Idempotent-Replayed") != "" {
		t.Fatal("first response must not be marked as replayed")
	}
	for i := 0; i < 5; i++ {
		again := p.s.Client.MustTransfer(t, key, p.req(2_500))
		if again.Status != first.Status || !bytes.Equal(again.Body, first.Body) {
			t.Fatalf("replay %d differs:\n got  %s\n want %s", i, again, first)
		}
		if again.Header.Get("Idempotent-Replayed") != "true" {
			t.Fatalf("replay %d not marked with Idempotent-Replayed: true", i)
		}
	}

	if n := count(t, p.s, `SELECT count(*) FROM transfers WHERE idempotency_key = $1`, key); n != 1 {
		t.Fatalf("%d transfers for the key, want 1", n)
	}
	if b := p.s.Client.Balance(t, p.from.ID); b != 7_500 {
		t.Fatalf("source balance %d, want 7500 (debited exactly once)", b)
	}
	if b := p.s.Client.Balance(t, p.to.ID); b != 2_500 {
		t.Fatalf("destination balance %d, want 2500", b)
	}
	requireInvariants(t, p.s)
}

func TestReplayIgnoresJSONFormatting(t *testing.T) {
	p := newPair(t)
	key := newKey(t)
	compact := fmt.Sprintf(`{"from_account":%q,"to_account":%q,"amount":100,"currency":"USD"}`, p.from.ID, p.to.ID)
	reordered := fmt.Sprintf("{\n  \"currency\": \"USD\",\n  \"amount\": 100,\n  \"to_account\": %q,\n  \"from_account\": %q\n}", p.to.ID, p.from.ID)

	first, err := p.s.Client.Do(context.Background(), http.MethodPost, "/v1/transfers", []byte(compact), "Idempotency-Key", key)
	if err != nil || first.Status != http.StatusCreated {
		t.Fatalf("first: %v %s", err, first)
	}
	again, err := p.s.Client.Do(context.Background(), http.MethodPost, "/v1/transfers", []byte(reordered), "Idempotency-Key", key)
	if err != nil {
		t.Fatal(err)
	}
	if again.Status != http.StatusCreated || !bytes.Equal(again.Body, first.Body) {
		t.Fatalf("semantically identical request was not replayed: %s", again)
	}
}

func TestSameKeyDifferentPayloadIs422(t *testing.T) {
	p := newPair(t)
	key := newKey(t)
	first := p.s.Client.MustTransfer(t, key, p.req(1_000))
	if first.Status != http.StatusCreated {
		t.Fatalf("first: %s", first)
	}
	other := p.s.Client.CreateAccount(t, "USD", 0)
	variants := map[string]ledgertest.TransferRequest{
		"amount":      p.req(1_001),
		"destination": {FromAccount: p.from.ID, ToAccount: other.ID, Amount: 1_000, Currency: "USD"},
		"direction":   {FromAccount: p.to.ID, ToAccount: p.from.ID, Amount: 1_000, Currency: "USD"},
		"currency":    {FromAccount: p.from.ID, ToAccount: p.to.ID, Amount: 1_000, Currency: "EUR"},
	}
	for name, req := range variants {
		resp := p.s.Client.MustTransfer(t, key, req)
		if resp.Status != http.StatusUnprocessableEntity || resp.ErrorCode() != "idempotency_key_reused" {
			t.Fatalf("%s changed: got %s, want 422 idempotency_key_reused", name, resp)
		}
	}
	// The original request still replays, and nothing else executed.
	if again := p.s.Client.MustTransfer(t, key, p.req(1_000)); !bytes.Equal(again.Body, first.Body) {
		t.Fatalf("original no longer replays: %s", again)
	}
	if n := count(t, p.s, `SELECT count(*) FROM transfers WHERE kind = 'transfer'`); n != 1 {
		t.Fatalf("%d transfers, want 1", n)
	}
	requireInvariants(t, p.s)
}

// TestSameKeyWhileInFlightIs409 holds a row lock on the source account from
// a separate connection, so the first request blocks mid-transaction while
// holding its key. A second request with the same key must be rejected with
// 409 at once, not queued and not executed.
func TestSameKeyWhileInFlightIs409(t *testing.T) {
	p := newPair(t)
	key := newKey(t)
	ctx := context.Background()

	blocker := p.s.DB.Conn(t)
	tx, err := blocker.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM accounts WHERE id = $1 FOR UPDATE`, p.from.ID); err != nil {
		t.Fatal(err)
	}

	firstDone := make(chan ledgertest.Response, 1)
	go func() {
		resp, err := p.s.Client.Transfer(ctx, key, p.req(500))
		if err != nil {
			t.Errorf("first request: %v", err)
		}
		firstDone <- resp
	}()
	waitForKeyLock(t, p.s.DB.Pool, key)

	second := p.s.Client.MustTransfer(t, key, p.req(500))
	if second.Status != http.StatusConflict || second.ErrorCode() != "idempotency_key_in_use" {
		t.Fatalf("second request: got %s, want 409 idempotency_key_in_use", second)
	}
	if second.Header.Get("Retry-After") == "" {
		t.Fatal("409 response lacks Retry-After")
	}
	// Even a different payload gets 409 while the key is in flight: the key
	// is busy, which is checked before the payload.
	if r := p.s.Client.MustTransfer(t, key, p.req(501)); r.Status != http.StatusConflict {
		t.Fatalf("different payload while in flight: %s", r)
	}

	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	first := <-firstDone
	if first.Status != http.StatusCreated {
		t.Fatalf("first request after release: %s", first)
	}
	third := p.s.Client.MustTransfer(t, key, p.req(500))
	if third.Status != http.StatusCreated || !bytes.Equal(third.Body, first.Body) {
		t.Fatalf("third request should replay the first: %s", third)
	}
	if b := p.s.Client.Balance(t, p.from.ID); b != 9_500 {
		t.Fatalf("source balance %d, want 9500", b)
	}
	requireInvariants(t, p.s)
}

// TestKeyRecordAndTransferCommitTogether makes storing the key record fail
// after the transfer and its entries were written. If they were separate
// transactions the transfer would survive; it must not.
func TestKeyRecordAndTransferCommitTogether(t *testing.T) {
	p := newPair(t)
	key := newKey(t)
	exec(t, p.s, `
		CREATE FUNCTION fail_key_insert() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'injected failure after the transfer was written'; END $$;
		CREATE TRIGGER fail_key_insert BEFORE INSERT ON idempotency_keys
			FOR EACH ROW EXECUTE FUNCTION fail_key_insert();`)

	resp := p.s.Client.MustTransfer(t, key, p.req(4_000))
	if resp.Status != http.StatusInternalServerError {
		t.Fatalf("got %s, want 500", resp)
	}
	if n := count(t, p.s, `SELECT count(*) FROM transfers WHERE kind = 'transfer'`); n != 0 {
		t.Fatalf("%d transfers survived the failed key insert", n)
	}
	if n := count(t, p.s, `SELECT count(*) FROM idempotency_keys`); n != 0 {
		t.Fatalf("%d key records stored", n)
	}
	if b := p.s.Client.Balance(t, p.from.ID); b != 10_000 {
		t.Fatalf("source balance %d, want 10000 untouched", b)
	}

	// After the fault is gone the same key executes exactly once.
	exec(t, p.s, `DROP TRIGGER fail_key_insert ON idempotency_keys`)
	if r := p.s.Client.MustTransfer(t, key, p.req(4_000)); r.Status != http.StatusCreated {
		t.Fatalf("retry: %s", r)
	}
	if r := p.s.Client.MustTransfer(t, key, p.req(4_000)); r.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("second retry not replayed: %s", r)
	}
	if b := p.s.Client.Balance(t, p.from.ID); b != 6_000 {
		t.Fatalf("source balance %d, want 6000", b)
	}
	requireInvariants(t, p.s)
}

// failFirstKeyInserts makes the first n idempotency-key inserts fail with the
// given SQLSTATE. A sequence counts attempts because it is not rolled back.
func failFirstKeyInserts(t *testing.T, s *ledgertest.Server, n int, sqlstate string) {
	t.Helper()
	exec(t, s, fmt.Sprintf(`
		CREATE SEQUENCE injected_failures;
		CREATE FUNCTION inject_failure() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF nextval('injected_failures') <= %d THEN
				RAISE EXCEPTION 'injected transient failure' USING ERRCODE = '%s';
			END IF;
			RETURN NEW;
		END $$;
		CREATE TRIGGER inject_failure BEFORE INSERT ON idempotency_keys
			FOR EACH ROW EXECUTE FUNCTION inject_failure();`, n, sqlstate))
}

func TestTransientFailuresAreRetriedTransparently(t *testing.T) {
	for _, tc := range []struct {
		name, sqlstate string
	}{
		{"serialization failure", "40001"},
		{"deadlock", "40P01"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPair(t)
			failFirstKeyInserts(t, p.s, 2, tc.sqlstate)
			before := p.s.App.Runner().Stats()

			resp := p.s.Client.MustTransfer(t, newKey(t), p.req(1_234))
			if resp.Status != http.StatusCreated {
				t.Fatalf("got %s, want 201 after transparent retries", resp)
			}
			after := p.s.App.Runner().Stats()
			if retried := after.Total() - before.Total(); retried != 2 {
				t.Fatalf("%d retried attempts, want 2 (stats %+v)", retried, after)
			}
			if n := count(t, p.s, `SELECT count(*) FROM transfers WHERE kind = 'transfer'`); n != 1 {
				t.Fatalf("%d transfers, want exactly 1 despite 3 attempts", n)
			}
			requireInvariants(t, p.s)
		})
	}
}

func TestExhaustedRetriesReturn503AndAreNotCached(t *testing.T) {
	p := newPair(t)
	failFirstKeyInserts(t, p.s, 1_000_000, "40001")
	key := newKey(t)

	resp := p.s.Client.MustTransfer(t, key, p.req(700))
	if resp.Status != http.StatusServiceUnavailable || resp.ErrorCode() != "transaction_conflict" {
		t.Fatalf("got %s, want 503 transaction_conflict", resp)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("503 response lacks Retry-After")
	}
	if s := p.s.App.Runner().Stats(); s.Exhausted != 1 || s.Serialization != int64(p.s.Config.TxMaxAttempts) {
		t.Fatalf("stats %+v, want %d attempts and one exhaustion", s, p.s.Config.TxMaxAttempts)
	}
	if n := count(t, p.s, `SELECT count(*) FROM transfers WHERE kind = 'transfer'`); n != 0 {
		t.Fatalf("%d transfers after a failed request", n)
	}

	// The failure was not cached: once the database recovers the same key works.
	exec(t, p.s, `DROP TRIGGER inject_failure ON idempotency_keys`)
	if r := p.s.Client.MustTransfer(t, key, p.req(700)); r.Status != http.StatusCreated || r.Header.Get("Idempotent-Replayed") != "" {
		t.Fatalf("retry after recovery: %s (replayed=%q)", r, r.Header.Get("Idempotent-Replayed"))
	}
	requireInvariants(t, p.s)
}

// TestBusinessRejectionsAreReplayed shows that a definitive rejection is
// stored: a retry with the same key cannot turn a rejected transfer into an
// executed one later, even after the account has been funded.
func TestBusinessRejectionsAreReplayed(t *testing.T) {
	p := newPair(t)
	key := newKey(t)
	poor := p.s.Client.CreateAccount(t, "USD", 100)
	req := ledgertest.TransferRequest{FromAccount: poor.ID, ToAccount: p.to.ID, Amount: 500, Currency: "USD"}

	first := p.s.Client.MustTransfer(t, key, req)
	if first.Status != http.StatusUnprocessableEntity || first.ErrorCode() != "insufficient_funds" {
		t.Fatalf("got %s, want 422 insufficient_funds", first)
	}
	// Fund the account so the transfer would now succeed.
	if r := p.s.Client.MustTransfer(t, newKey(t), ledgertest.TransferRequest{FromAccount: p.from.ID, ToAccount: poor.ID, Amount: 1_000, Currency: "USD"}); r.Status != http.StatusCreated {
		t.Fatalf("funding: %s", r)
	}
	again := p.s.Client.MustTransfer(t, key, req)
	if again.Status != first.Status || !bytes.Equal(again.Body, first.Body) || again.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("rejection not replayed: %s", again)
	}
	if b := p.s.Client.Balance(t, poor.ID); b != 1_100 {
		t.Fatalf("balance %d, want 1100: the rejected transfer must not execute", b)
	}
	// A new key is a new request and succeeds.
	if r := p.s.Client.MustTransfer(t, newKey(t), req); r.Status != http.StatusCreated {
		t.Fatalf("new key: %s", r)
	}
	requireInvariants(t, p.s)
}

func expireKey(t *testing.T, s *ledgertest.Server, key string) {
	t.Helper()
	exec(t, s, `UPDATE idempotency_keys SET created_at = now() - interval '2 days', expires_at = now() - interval '1 day' WHERE key = $1`, key)
}

// TestExpiredKeyNeverMovesMoneyTwice: the TTL bounds how long responses are
// cached, not how often a key can move money. After expiry (and cleanup) the
// same request gets the original response rebuilt from the immutable
// transfer; a different request is still rejected.
func TestExpiredKeyNeverMovesMoneyTwice(t *testing.T) {
	p := newPair(t)
	key := newKey(t)
	first := p.s.Client.MustTransfer(t, key, p.req(3_000))
	if first.Status != http.StatusCreated {
		t.Fatalf("first: %s", first)
	}
	expireKey(t, p.s, key)
	if n, err := p.s.App.Store().DeleteExpired(context.Background(), 100); err != nil || n != 1 {
		t.Fatalf("cleanup deleted %d (err %v), want 1", n, err)
	}

	rebuilt := p.s.Client.MustTransfer(t, key, p.req(3_000))
	if rebuilt.Status != http.StatusCreated || !bytes.Equal(rebuilt.Body, first.Body) {
		t.Fatalf("rebuilt response differs:\n got  %s\n want %s", rebuilt, first)
	}
	if rebuilt.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatal("rebuilt response not marked as replayed")
	}
	if r := p.s.Client.MustTransfer(t, key, p.req(3_001)); r.Status != http.StatusUnprocessableEntity || r.ErrorCode() != "idempotency_key_reused" {
		t.Fatalf("different payload on expired key: %s", r)
	}
	expireKey(t, p.s, key) // expired but not yet cleaned up: same behaviour
	if r := p.s.Client.MustTransfer(t, key, p.req(3_000)); !bytes.Equal(r.Body, first.Body) {
		t.Fatalf("expired-not-deleted key: %s", r)
	}
	if b := p.s.Client.Balance(t, p.from.ID); b != 7_000 {
		t.Fatalf("source balance %d, want 7000", b)
	}
	requireInvariants(t, p.s)
}

// TestExpiredRejectionMayExecute documents the other half of the TTL rule:
// a stored rejection moved no money, so once it expires the key is free.
func TestExpiredRejectionMayExecute(t *testing.T) {
	p := newPair(t)
	key := newKey(t)
	req := p.req(20_000) // more than the 10,000 available
	if r := p.s.Client.MustTransfer(t, key, req); r.ErrorCode() != "insufficient_funds" {
		t.Fatalf("first: %s", r)
	}
	if r := p.s.Client.MustTransfer(t, newKey(t), ledgertest.TransferRequest{FromAccount: p.from.ID, ToAccount: p.to.ID, Amount: 1, Currency: "USD"}); r.Status != http.StatusCreated {
		t.Fatalf("unrelated transfer: %s", r)
	}
	richer := p.s.Client.CreateAccount(t, "USD", 50_000)
	if r := p.s.Client.MustTransfer(t, newKey(t), ledgertest.TransferRequest{FromAccount: richer.ID, ToAccount: p.from.ID, Amount: 50_000, Currency: "USD"}); r.Status != http.StatusCreated {
		t.Fatalf("top-up: %s", r)
	}
	expireKey(t, p.s, key)
	if r := p.s.Client.MustTransfer(t, key, req); r.Status != http.StatusCreated {
		t.Fatalf("after expiry: %s, want 201", r)
	}
	requireInvariants(t, p.s)
}

// TestConcurrentRequestsWithOneKeyExecuteOnce fires many identical requests
// at once. Each gets either the single execution's response (fresh or
// replayed) or a 409, and exactly one transfer exists afterwards.
func TestConcurrentRequestsWithOneKeyExecuteOnce(t *testing.T) {
	p := newPair(t)
	key := newKey(t)
	const n = 50
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		bodies   [][]byte
		conflict int
	)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			resp, err := p.s.Client.Transfer(context.Background(), key, p.req(10))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				t.Errorf("request: %v", err)
			case resp.Status == http.StatusCreated:
				bodies = append(bodies, resp.Body)
			case resp.Status == http.StatusConflict:
				conflict++
			default:
				t.Errorf("unexpected response %s", resp)
			}
		}()
	}
	close(start)
	wg.Wait()

	final := p.s.Client.MustTransfer(t, key, p.req(10))
	if final.Status != http.StatusCreated {
		t.Fatalf("final: %s", final)
	}
	for _, b := range bodies {
		if !bytes.Equal(b, final.Body) {
			t.Fatalf("responses for one key differ:\n%s\n%s", b, final.Body)
		}
	}
	if c := count(t, p.s, `SELECT count(*) FROM transfers WHERE idempotency_key = $1`, key); c != 1 {
		t.Fatalf("%d transfers for one key", c)
	}
	if b := p.s.Client.Balance(t, p.from.ID); b != 9_990 {
		t.Fatalf("source balance %d, want 9990", b)
	}
	t.Logf("%d x 201, %d x 409", len(bodies), conflict)
	requireInvariants(t, p.s)
}

func TestIdempotencyKeyIsRequiredAndValidated(t *testing.T) {
	p := newPair(t)
	for name, header := range map[string][]string{
		"missing":  nil,
		"empty":    {"Idempotency-Key", ""},
		"spaces":   {"Idempotency-Key", "has spaces"},
		"too long": {"Idempotency-Key", string(bytes.Repeat([]byte("k"), 256))},
	} {
		resp, err := p.s.Client.Do(context.Background(), http.MethodPost, "/v1/transfers", p.req(1), header...)
		if err != nil {
			t.Fatal(err)
		}
		if resp.Status != http.StatusBadRequest || resp.ErrorCode() != "invalid_idempotency_key" {
			t.Fatalf("%s key: got %s, want 400 invalid_idempotency_key", name, resp)
		}
	}
	if n := count(t, p.s, `SELECT count(*) FROM transfers WHERE kind = 'transfer'`); n != 0 {
		t.Fatalf("%d transfers executed without a valid key", n)
	}
}

func TestAccountCreationIsIdempotentWithAKey(t *testing.T) {
	s := ledgertest.NewServer(t)
	ctx := context.Background()
	key := newKey(t)
	body := map[string]any{"currency": "EUR", "initial_balance": 5_000}

	first, err := s.Client.Do(ctx, http.MethodPost, "/v1/accounts", body, "Idempotency-Key", key)
	if err != nil || first.Status != http.StatusCreated {
		t.Fatalf("first: %v %s", err, first)
	}
	again, err := s.Client.Do(ctx, http.MethodPost, "/v1/accounts", body, "Idempotency-Key", key)
	if err != nil || !bytes.Equal(again.Body, first.Body) || again.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: %v %s", err, again)
	}
	other, err := s.Client.Do(ctx, http.MethodPost, "/v1/accounts", map[string]any{"currency": "EUR", "initial_balance": 6_000}, "Idempotency-Key", key)
	if err != nil || other.Status != http.StatusUnprocessableEntity {
		t.Fatalf("different body: %v %s", err, other)
	}
	if n := count(t, s, `SELECT count(*) FROM accounts WHERE NOT is_system`); n != 1 {
		t.Fatalf("%d user accounts, want 1", n)
	}
	if n := count(t, s, `SELECT count(*) FROM transfers WHERE kind = 'funding'`); n != 1 {
		t.Fatalf("%d funding transfers, want 1", n)
	}
	// Using a transfer key for an account (or vice versa) is a different request.
	if r, _ := s.Client.Do(ctx, http.MethodPost, "/v1/transfers", ledgertest.TransferRequest{
		FromAccount: "00000000-0000-0000-0000-000000000001", ToAccount: "00000000-0000-0000-0000-000000000002", Amount: 1, Currency: "EUR",
	}, "Idempotency-Key", key); r.Status != http.StatusUnprocessableEntity || r.ErrorCode() != "idempotency_key_reused" {
		t.Fatalf("cross-endpoint key reuse: %s", r)
	}
	requireInvariants(t, s)
}

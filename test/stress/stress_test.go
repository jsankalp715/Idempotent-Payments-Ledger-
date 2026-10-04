// Package stress_test hammers the ledger over real HTTP with concurrent
// transfers while injecting the failures idempotency exists for: responses
// lost after the server committed, client timeouts at random points, requests
// abandoned before they are sent, duplicate submissions racing each other and
// late duplicates. Afterwards it checks that every key was applied at most
// once, that the client's own bookkeeping matches the ledger to the unit, and
// that the global invariants hold.
//
// Size is configurable through the environment:
//
//	STRESS_MODE=ci|large   preset (default ci; large = 12,000 transfers, 200 goroutines)
//	STRESS_TRANSFERS, STRESS_WORKERS, STRESS_ACCOUNTS, STRESS_INITIAL_BALANCE,
//	STRESS_MAX_AMOUNT, STRESS_DB_CONNS, STRESS_SEED  override single values
//	TX_ISOLATION=serializable  run the service with SERIALIZABLE transactions
package stress_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jsankalp715/Idempotent-Payments-Ledger-/internal/ledgertest"
)

type stressConfig struct {
	Mode           string
	Transfers      int
	Workers        int
	Accounts       int
	InitialBalance int64
	MaxAmount      int64
	DBConns        int
	Seed           uint64
}

// Failure-injection probabilities, per transfer.
const (
	pResponseLost  = 0.10 // server processes the request, the client never sees the reply
	pClientTimeout = 0.10 // client deadline of 0.5 to 15 ms: cancels at a random stage
	pPreCancelled  = 0.03 // context cancelled before the request is even sent
	pConcurrentDup = 0.10 // 2 to 4 identical requests fired at the same moment
	pLateDuplicate = 0.10 // the request is sent again after its definitive answer
	pHugeAmount    = 0.02 // an amount no account can afford: a guaranteed rejection
	maxAttempts    = 200  // per transfer, before the test gives up on it
)

func loadConfig(t *testing.T) stressConfig {
	t.Helper()
	cfg := stressConfig{Mode: "ci", Transfers: 3000, Workers: 64, Accounts: 20, InitialBalance: 10_000, MaxAmount: 2_000, DBConns: 20}
	if os.Getenv("STRESS_MODE") == "large" {
		cfg = stressConfig{Mode: "large", Transfers: 12_000, Workers: 200, Accounts: 20, InitialBalance: 10_000, MaxAmount: 2_000, DBConns: 20}
	}
	cfg.Seed = uint64(time.Now().UnixNano())
	envInt := func(name string, dst *int) {
		if v := os.Getenv(name); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				t.Fatalf("%s=%q: want a positive integer", name, v)
			}
			*dst = n
		}
	}
	envInt("STRESS_TRANSFERS", &cfg.Transfers)
	envInt("STRESS_WORKERS", &cfg.Workers)
	envInt("STRESS_ACCOUNTS", &cfg.Accounts)
	envInt("STRESS_DB_CONNS", &cfg.DBConns)
	var initial, maxAmount int
	envInt("STRESS_INITIAL_BALANCE", &initial)
	envInt("STRESS_MAX_AMOUNT", &maxAmount)
	if initial > 0 {
		cfg.InitialBalance = int64(initial)
	}
	if maxAmount > 0 {
		cfg.MaxAmount = int64(maxAmount)
	}
	if v := os.Getenv("STRESS_SEED"); v != "" {
		s, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			t.Fatalf("STRESS_SEED=%q: %v", v, err)
		}
		cfg.Seed = s
	}
	if cfg.Accounts < 2 {
		t.Fatal("STRESS_ACCOUNTS must be at least 2")
	}
	return cfg
}

type job struct {
	key           string
	req           ledgertest.TransferRequest
	loseResponse  bool
	clientTimeout time.Duration
	preCancelled  bool
	concurrentDup int
	lateDuplicate bool
}

type outcome struct {
	final    ledgertest.Response   // the definitive answer (201, or 422 insufficient_funds)
	observed []ledgertest.Response // every definitive answer seen for this key
	err      error                 // set if no definitive answer was obtained
}

// stats are counted across all workers.
type stats struct {
	sent, lost, timedOut, preCancelled, transportErrors atomic.Int64
	inFlight409, unavailable503, replays             atomic.Int64
}

type loseKey struct{}

var errResponseLost = errors.New("simulated network failure: response lost")

// chaosTransport delivers the request and lets the server finish, then throws
// the response away when the request context asks for it: the classic "did my
// payment go through?" failure.
type chaosTransport struct{ base http.RoundTripper }

func (c chaosTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := c.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if lose, _ := req.Context().Value(loseKey{}).(bool); lose {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return nil, errResponseLost
	}
	return resp, nil
}

func TestStress(t *testing.T) {
	cfg := loadConfig(t)
	srv := ledgertest.NewServer(t, ledgertest.Options{MaxConns: int32(cfg.DBConns)})
	t.Logf("config %+v isolation=%s (reproduce the job list with STRESS_SEED=%d)", cfg, srv.Config.TxIsolation, cfg.Seed)

	transport := &http.Transport{MaxIdleConns: 4 * cfg.Workers, MaxIdleConnsPerHost: 4 * cfg.Workers, IdleConnTimeout: time.Minute}
	defer transport.CloseIdleConnections()
	client := ledgertest.NewClient(srv.HTTP.URL, &http.Client{Transport: chaosTransport{base: transport}})

	accounts := make([]string, cfg.Accounts)
	for i := range accounts {
		accounts[i] = srv.Client.CreateAccount(t, "USD", cfg.InitialBalance).ID
	}
	jobs := makeJobs(cfg, accounts)

	ctx, cancel := context.WithTimeout(context.Background(), 9*time.Minute)
	defer cancel()
	var st stats
	outcomes := make([]outcome, len(jobs))
	queue := make(chan int)
	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < cfg.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(cfg.Seed, uint64(w)+1))
			for i := range queue {
				outcomes[i] = execute(ctx, client, jobs[i], rng, &st)
			}
		}()
	}
	for i := range jobs {
		queue <- i
	}
	close(queue)
	wg.Wait()
	elapsed := time.Since(start)
	if ctx.Err() != nil {
		t.Fatalf("stress run did not finish within its time limit")
	}

	verify(t, cfg, srv, jobs, outcomes, accounts)

	retries := srv.App.Runner().Stats()
	t.Logf("%d transfers in %v (%.0f/s) by %d goroutines over %d accounts",
		len(jobs), elapsed.Round(time.Millisecond), float64(len(jobs))/elapsed.Seconds(), cfg.Workers, cfg.Accounts)
	t.Logf("requests sent %d: responses lost %d, client timeouts %d, pre-cancelled %d, transport errors %d, 409 in flight %d, 503 transient %d, replays %d",
		st.sent.Load(), st.lost.Load(), st.timedOut.Load(), st.preCancelled.Load(), st.transportErrors.Load(),
		st.inFlight409.Load(), st.unavailable503.Load(), st.replays.Load())
	t.Logf("server-side transaction retries: %+v", retries)
}

func makeJobs(cfg stressConfig, accounts []string) []job {
	rng := rand.New(rand.NewPCG(cfg.Seed, 0))
	runID := strconv.FormatUint(cfg.Seed, 36) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	jobs := make([]job, cfg.Transfers)
	for i := range jobs {
		from := rng.IntN(len(accounts))
		to := rng.IntN(len(accounts) - 1)
		if to >= from {
			to++
		}
		amount := rng.Int64N(cfg.MaxAmount) + 1
		if rng.Float64() < pHugeAmount {
			amount = cfg.InitialBalance * int64(len(accounts)) * 2
		}
		j := job{
			key: fmt.Sprintf("stress-%s-%d", runID, i),
			req: ledgertest.TransferRequest{FromAccount: accounts[from], ToAccount: accounts[to], Amount: amount, Currency: "USD"},
		}
		switch p := rng.Float64(); {
		case p < pResponseLost:
			j.loseResponse = true
		case p < pResponseLost+pClientTimeout:
			j.clientTimeout = time.Duration(500+rng.IntN(14_500)) * time.Microsecond
		case p < pResponseLost+pClientTimeout+pPreCancelled:
			j.preCancelled = true
		case p < pResponseLost+pClientTimeout+pPreCancelled+pConcurrentDup:
			j.concurrentDup = 2 + rng.IntN(3)
		}
		j.lateDuplicate = rng.Float64() < pLateDuplicate
		jobs[i] = j
	}
	return jobs
}

// execute drives one transfer to a definitive answer like a careful client:
// the first attempt may be sabotaged, then it retries with the same key on
// errors, 409 and 503 until it gets 201 or 422.
func execute(ctx context.Context, c *ledgertest.Client, j job, rng *rand.Rand, st *stats) outcome {
	var out outcome
	observe := func(r ledgertest.Response) {
		if r.Header.Get("Idempotent-Replayed") == "true" {
			st.replays.Add(1)
		}
		if r.Status == http.StatusCreated || r.Status == http.StatusUnprocessableEntity {
			out.observed = append(out.observed, r)
		}
	}
	send := func(ctx context.Context) (ledgertest.Response, error) {
		st.sent.Add(1)
		return c.Transfer(ctx, j.key, j.req)
	}

	switch {
	case j.loseResponse:
		if _, err := send(context.WithValue(ctx, loseKey{}, true)); errors.Is(err, errResponseLost) {
			st.lost.Add(1)
		}
	case j.clientTimeout > 0:
		tctx, cancel := context.WithTimeout(ctx, j.clientTimeout)
		resp, err := send(tctx)
		cancel()
		if err != nil {
			st.timedOut.Add(1)
		} else {
			observe(resp)
		}
	case j.preCancelled:
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := send(cctx); err != nil {
			st.preCancelled.Add(1)
		}
	case j.concurrentDup > 0:
		var mu sync.Mutex
		var wg sync.WaitGroup
		for k := 0; k < j.concurrentDup; k++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				resp, err := send(ctx)
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err != nil:
					st.transportErrors.Add(1)
				case resp.Status == http.StatusConflict:
					st.inFlight409.Add(1)
				default:
					observe(resp)
				}
			}()
		}
		wg.Wait()
	}

	for attempt := 1; ; attempt++ {
		if attempt > maxAttempts {
			out.err = fmt.Errorf("key %s: no definitive answer after %d attempts", j.key, maxAttempts)
			return out
		}
		if ctx.Err() != nil {
			out.err = fmt.Errorf("key %s: %w", j.key, ctx.Err())
			return out
		}
		resp, err := send(ctx)
		if err != nil {
			st.transportErrors.Add(1)
			backoff(ctx, rng, attempt)
			continue
		}
		switch resp.Status {
		case http.StatusCreated, http.StatusUnprocessableEntity:
			observe(resp)
			out.final = resp
		case http.StatusConflict:
			st.inFlight409.Add(1)
			backoff(ctx, rng, attempt)
			continue
		case http.StatusServiceUnavailable:
			st.unavailable503.Add(1)
			backoff(ctx, rng, attempt)
			continue
		default:
			out.err = fmt.Errorf("key %s: unexpected response %s", j.key, resp)
			return out
		}
		break
	}

	if j.lateDuplicate {
		resp, err := send(ctx)
		if err != nil {
			out.err = fmt.Errorf("key %s: late duplicate: %w", j.key, err)
			return out
		}
		if resp.Header.Get("Idempotent-Replayed") != "true" {
			out.err = fmt.Errorf("key %s: late duplicate was not a replay: %s", j.key, resp)
			return out
		}
		observe(resp)
	}
	return out
}

func backoff(ctx context.Context, rng *rand.Rand, attempt int) {
	d := time.Duration(1+rng.IntN(min(attempt, 20)*2)) * time.Millisecond
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
}

func verify(t *testing.T, cfg stressConfig, srv *ledgertest.Server, jobs []job, outcomes []outcome, accounts []string) {
	t.Helper()
	ctx := context.Background()

	// 1. Every key reached a definitive answer, and every answer seen for a
	//    key (first execution, concurrent duplicates, replays, late
	//    duplicates) is byte-for-byte the same response.
	expected := make(map[string]int64, len(accounts))
	for _, id := range accounts {
		expected[id] = cfg.InitialBalance
	}
	succeeded := map[string]string{} // key -> transfer id, from the client's point of view
	var rejected int
	failures := 0
	for i, o := range outcomes {
		j := jobs[i]
		if o.err != nil {
			failures++
			if failures <= 10 {
				t.Errorf("%v", o.err)
			}
			continue
		}
		for _, r := range o.observed {
			if r.Status != o.final.Status || !bytes.Equal(r.Body, o.final.Body) {
				t.Errorf("key %s: answers differ:\n %s\n %s", j.key, r, o.final)
			}
		}
		switch {
		case o.final.Status == http.StatusCreated:
			var tr ledgertest.Transfer
			if err := json.Unmarshal(o.final.Body, &tr); err != nil {
				t.Fatalf("key %s: %v", j.key, err)
			}
			if tr.FromAccount != j.req.FromAccount || tr.ToAccount != j.req.ToAccount || tr.Amount != j.req.Amount {
				t.Errorf("key %s: response describes a different transfer: %s", j.key, o.final)
			}
			succeeded[j.key] = tr.ID
			expected[j.req.FromAccount] -= j.req.Amount
			expected[j.req.ToAccount] += j.req.Amount
		case o.final.ErrorCode() == "insufficient_funds":
			rejected++
		default:
			t.Errorf("key %s: unexpected definitive answer %s", j.key, o.final)
		}
	}
	if failures > 0 {
		t.Fatalf("%d transfers never got a definitive answer", failures)
	}
	t.Logf("outcomes: %d executed, %d rejected for insufficient funds", len(succeeded), rejected)
	if len(succeeded) == 0 || rejected == 0 {
		t.Errorf("degenerate run: want both executed and rejected transfers")
	}

	// 2. The database holds exactly the transfers the client was told about:
	//    same keys, same transfer ids, nothing extra (e: at most once).
	rows, err := srv.DB.Pool.Query(ctx, `SELECT idempotency_key, id::text FROM transfers WHERE kind = 'transfer'`)
	if err != nil {
		t.Fatal(err)
	}
	inDB := map[string]string{}
	for rows.Next() {
		var key, id string
		if err := rows.Scan(&key, &id); err != nil {
			t.Fatal(err)
		}
		if prev, dup := inDB[key]; dup {
			t.Errorf("key %s applied twice: %s and %s", key, prev, id)
		}
		inDB[key] = id
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for key, id := range succeeded {
		if inDB[key] != id {
			t.Errorf("key %s: client saw transfer %s, database has %q", key, id, inDB[key])
		}
	}
	for key, id := range inDB {
		if _, ok := succeeded[key]; !ok {
			t.Errorf("key %s: database has transfer %s the client was never told about", key, id)
		}
	}
	var storedKeys int
	if err := srv.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM idempotency_keys WHERE request_path = '/v1/transfers'`).Scan(&storedKeys); err != nil {
		t.Fatal(err)
	}
	if storedKeys != len(jobs) {
		t.Errorf("%d idempotency records for %d keys", storedKeys, len(jobs))
	}

	// 3. The client's independent bookkeeping matches every balance exactly,
	//    and money was conserved (a): user balances still add up to what was
	//    funded and none is negative (b).
	var total int64
	ids := make([]string, 0, len(accounts))
	ids = append(ids, accounts...)
	sort.Strings(ids)
	for _, id := range ids {
		got := srv.Client.Balance(t, id)
		if got != expected[id] {
			t.Errorf("account %s: balance %d, client bookkeeping says %d", id, got, expected[id])
		}
		if got < 0 {
			t.Errorf("account %s: negative balance %d", id, got)
		}
		total += got
	}
	if want := cfg.InitialBalance * int64(len(accounts)); total != want {
		t.Errorf("user balances sum to %d, want %d: money was created or destroyed", total, want)
	}

	// 4. Global invariants (a)-(e), checked with independent SQL.
	summary, err := ledgertest.CheckInvariants(ctx, srv.DB.Pool)
	if err != nil {
		t.Fatalf("ledger invariants violated:\n%v", err)
	}
	t.Logf("invariants hold over %d accounts, %d transfers, %d entries, %d idempotency records",
		summary.Accounts, summary.Transfers, summary.Entries, summary.IdempotentKeys)
}

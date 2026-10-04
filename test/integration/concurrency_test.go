package integration_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jsankalp715/Idempotent-Payments-Ledger-/internal/ledgertest"
)

// TestOverdraftRace drains one account with many concurrent transfers. The
// row lock serializes them and the balance check runs under the lock, so
// exactly floor(balance / amount) succeed and the balance never goes negative.
func TestOverdraftRace(t *testing.T) {
	cases := []struct {
		balance, amount int64
		requests        int
	}{
		{balance: 1_000, amount: 30, requests: 60},  // 33 affordable
		{balance: 1_000, amount: 100, requests: 50}, // 10 affordable, exact drain
		{balance: 999, amount: 1_000, requests: 20}, // none affordable
		{balance: 5_000, amount: 7, requests: 200},  // 200 affordable: no rejections
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("balance=%d,amount=%d,n=%d", tc.balance, tc.amount, tc.requests), func(t *testing.T) {
			s := ledgertest.NewServer(t, ledgertest.Options{MaxConns: 30})
			src := s.Client.CreateAccount(t, "USD", tc.balance)
			dsts := make([]ledgertest.Account, 4)
			for i := range dsts {
				dsts[i] = s.Client.CreateAccount(t, "USD", 0)
			}

			var ok, rejected atomic.Int64
			var wg sync.WaitGroup
			start := make(chan struct{})
			for i := 0; i < tc.requests; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					key := newKey(t)
					<-start
					resp := transferUntilDefinitive(t, s.Client, key, ledgertest.TransferRequest{
						FromAccount: src.ID, ToAccount: dsts[i%len(dsts)].ID, Amount: tc.amount, Currency: "USD",
					})
					switch {
					case resp.Status == http.StatusCreated:
						ok.Add(1)
					case resp.ErrorCode() == "insufficient_funds":
						rejected.Add(1)
					default:
						t.Errorf("unexpected outcome %s", resp)
					}
				}()
			}
			close(start)
			wg.Wait()

			affordable := min(int64(tc.requests), tc.balance/tc.amount)
			if ok.Load() != affordable || rejected.Load() != int64(tc.requests)-affordable {
				t.Fatalf("%d succeeded and %d were rejected, want exactly %d and %d",
					ok.Load(), rejected.Load(), affordable, int64(tc.requests)-affordable)
			}
			if b := s.Client.Balance(t, src.ID); b != tc.balance-affordable*tc.amount {
				t.Fatalf("source balance %d, want %d", b, tc.balance-affordable*tc.amount)
			}
			var received int64
			for _, d := range dsts {
				received += s.Client.Balance(t, d.ID)
			}
			if received != affordable*tc.amount {
				t.Fatalf("destinations received %d, want %d", received, affordable*tc.amount)
			}
			requireInvariants(t, s)
		})
	}
}

// TestOpposingTransfersDoNotDeadlock runs A->B and B->A transfers in
// parallel. Locking in id order means two transfers over the same pair always
// request the locks in the same sequence, so PostgreSQL never sees a lock
// cycle: the runner observes zero deadlock errors and every transfer succeeds.
// (internal/postgres TestTxRunnerRecoversFromARealDeadlock shows the cycle,
// and its recovery, when two transactions lock in opposite order.)
func TestOpposingTransfersDoNotDeadlock(t *testing.T) {
	s := ledgertest.NewServer(t, ledgertest.Options{MaxConns: 40})
	a := s.Client.CreateAccount(t, "USD", 1_000_000)
	b := s.Client.CreateAccount(t, "USD", 1_000_000)
	before := s.App.Runner().Stats()

	const workers, perWorker = 32, 25
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var (
		wg              sync.WaitGroup
		moved           [2]atomic.Int64 // total moved A->B and B->A
		createdTransfer atomic.Int64
	)
	start := make(chan struct{})
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < perWorker && ctx.Err() == nil; i++ {
				dir := (w + i) % 2
				from, to := a.ID, b.ID
				if dir == 1 {
					from, to = b.ID, a.ID
				}
				amount := int64(rand.IntN(100) + 1)
				resp := transferUntilDefinitive(t, s.Client, newKey(t), ledgertest.TransferRequest{FromAccount: from, ToAccount: to, Amount: amount, Currency: "USD"})
				if resp.Status != http.StatusCreated {
					t.Errorf("transfer failed: %s", resp)
					return
				}
				moved[dir].Add(amount)
				createdTransfer.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if ctx.Err() != nil {
		t.Fatal("opposing transfers did not finish in time: possible deadlock")
	}

	if createdTransfer.Load() != workers*perWorker {
		t.Fatalf("%d transfers succeeded, want %d", createdTransfer.Load(), workers*perWorker)
	}
	after := s.App.Runner().Stats()
	if d := after.Deadlock - before.Deadlock; d != 0 {
		t.Fatalf("%d deadlocks detected; ordered locking should make them impossible", d)
	}
	balA, balB := s.Client.Balance(t, a.ID), s.Client.Balance(t, b.ID)
	if balA+balB != 2_000_000 {
		t.Fatalf("A+B = %d, want 2,000,000 (money created or destroyed)", balA+balB)
	}
	if want := 1_000_000 - moved[0].Load() + moved[1].Load(); balA != want {
		t.Fatalf("A = %d, want %d from the client's own bookkeeping", balA, want)
	}
	t.Logf("%d transfers, retries %+v", createdTransfer.Load(), after)
	requireInvariants(t, s)
}

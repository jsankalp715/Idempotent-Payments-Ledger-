package ledger_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jsankalp715/Idempotent-Payments-Ledger-/internal/ledger"
	"github.com/jsankalp715/Idempotent-Payments-Ledger-/internal/postgres"
	"github.com/jsankalp715/Idempotent-Payments-Ledger-/internal/testdb"
)

type env struct {
	db     *testdb.DB
	runner *postgres.TxRunner
	svc    *ledger.Service
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db := testdb.New(t)
	return &env{
		db:     db,
		runner: postgres.NewTxRunner(db.Pool, postgres.TxOptions{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond}),
		svc:    ledger.NewService(),
	}
}

func (e *env) createAccount(t *testing.T, currency string, initial int64) ledger.Account {
	t.Helper()
	acc, err := postgres.InTx(context.Background(), e.runner, func(ctx context.Context, tx pgx.Tx) (ledger.Account, error) {
		return e.svc.CreateAccount(ctx, tx, ledger.CreateAccountInput{Currency: currency, InitialBalance: initial})
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	return acc
}

func (e *env) transfer(from, to uuid.UUID, amount int64, currency, key string) (ledger.Transfer, error) {
	hash := sha256.Sum256([]byte(key))
	return postgres.InTx(context.Background(), e.runner, func(ctx context.Context, tx pgx.Tx) (ledger.Transfer, error) {
		return e.svc.Transfer(ctx, tx, ledger.TransferInput{FromAccountID: from, ToAccountID: to, Amount: amount, Currency: currency}, key, hash[:])
	})
}

func (e *env) balance(t *testing.T, id uuid.UUID) int64 {
	t.Helper()
	acc, err := e.svc.GetAccount(context.Background(), e.db.Pool, id)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	return acc.Balance
}

func (e *env) count(t *testing.T, table string) int {
	t.Helper()
	var n int
	if err := e.db.Pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCreateAccount(t *testing.T) {
	e := newEnv(t)
	empty := e.createAccount(t, "USD", 0)
	if empty.Balance != 0 || empty.Kind != ledger.AccountUser || empty.Currency != "USD" || empty.ID == uuid.Nil {
		t.Fatalf("unexpected account %+v", empty)
	}
	if e.count(t, "transfers") != 0 {
		t.Fatal("unfunded account created a transfer")
	}

	funded := e.createAccount(t, "USD", 2500)
	if funded.Balance != 2500 {
		t.Fatalf("funded balance = %d, want 2500", funded.Balance)
	}
	e.createAccount(t, "USD", 500)

	var systemID uuid.UUID
	var systemBalance int64
	if err := e.db.Pool.QueryRow(context.Background(),
		`SELECT id, balance FROM accounts WHERE is_system AND currency = 'USD'`).Scan(&systemID, &systemBalance); err != nil {
		t.Fatal(err)
	}
	if systemBalance != -3000 {
		t.Fatalf("system balance = %d, want -3000", systemBalance)
	}
	system, err := e.svc.GetAccount(context.Background(), e.db.Pool, systemID)
	if err != nil || system.Kind != ledger.AccountSystem {
		t.Fatalf("system account: %+v, %v", system, err)
	}
	if e.count(t, "transfers") != 2 || e.count(t, "entries") != 4 {
		t.Fatalf("want 2 funding transfers with 4 entries, got %d and %d", e.count(t, "transfers"), e.count(t, "entries"))
	}
}

func TestCreateAccountRejectsInvalidInput(t *testing.T) {
	e := newEnv(t)
	_, err := postgres.InTx(context.Background(), e.runner, func(ctx context.Context, tx pgx.Tx) (ledger.Account, error) {
		return e.svc.CreateAccount(ctx, tx, ledger.CreateAccountInput{Currency: "usd", InitialBalance: -1})
	})
	var v *ledger.ValidationError
	if !errors.As(err, &v) || len(v.Fields) != 2 {
		t.Fatalf("err = %v, want a validation error with two problems", err)
	}
}

func TestTransferMovesMoneyWithTwoEntries(t *testing.T) {
	e := newEnv(t)
	alice := e.createAccount(t, "EUR", 1000)
	bob := e.createAccount(t, "EUR", 0)

	tr, err := e.transfer(alice.ID, bob.ID, 300, "EUR", "k-1")
	if err != nil {
		t.Fatalf("transfer: %v", err)
	}
	if tr.Kind != ledger.KindTransfer || tr.Amount != 300 || tr.FromAccountID != alice.ID || tr.ToAccountID != bob.ID {
		t.Fatalf("unexpected transfer %+v", tr)
	}
	if len(tr.Entries) != 2 {
		t.Fatalf("got %d entries", len(tr.Entries))
	}
	debit, credit := tr.Entries[0], tr.Entries[1]
	if debit.AccountID != alice.ID || debit.Amount != -300 || debit.BalanceAfter != 700 || debit.Direction() != "debit" {
		t.Fatalf("unexpected debit %+v", debit)
	}
	if credit.AccountID != bob.ID || credit.Amount != 300 || credit.BalanceAfter != 300 || credit.Direction() != "credit" {
		t.Fatalf("unexpected credit %+v", credit)
	}
	if e.balance(t, alice.ID) != 700 || e.balance(t, bob.ID) != 300 {
		t.Fatalf("balances %d/%d, want 700/300", e.balance(t, alice.ID), e.balance(t, bob.ID))
	}

	stored, err := e.svc.GetTransfer(context.Background(), e.db.Pool, tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ID != tr.ID || len(stored.Entries) != 2 || stored.Entries[0].ID != debit.ID || stored.Entries[1].ID != credit.ID ||
		!stored.CreatedAt.Equal(tr.CreatedAt) {
		t.Fatalf("stored transfer differs: %+v vs %+v", stored, tr)
	}

	byKey, hash, found, err := e.svc.TransferByKey(context.Background(), e.db.Pool, "k-1")
	want := sha256.Sum256([]byte("k-1"))
	if err != nil || !found || byKey.ID != tr.ID || string(hash) != string(want[:]) {
		t.Fatalf("TransferByKey: found=%v id=%v err=%v", found, byKey.ID, err)
	}
	if _, _, found, err := e.svc.TransferByKey(context.Background(), e.db.Pool, "unknown"); err != nil || found {
		t.Fatalf("TransferByKey(unknown): found=%v err=%v", found, err)
	}
}

func TestTransferBusinessRuleFailuresWriteNothing(t *testing.T) {
	e := newEnv(t)
	usd := e.createAccount(t, "USD", 100)
	usd2 := e.createAccount(t, "USD", 0)
	eur := e.createAccount(t, "EUR", 100)
	var system uuid.UUID
	if err := e.db.Pool.QueryRow(context.Background(), `SELECT id FROM accounts WHERE is_system AND currency = 'USD'`).Scan(&system); err != nil {
		t.Fatal(err)
	}
	transfersBefore := e.count(t, "transfers")

	cases := []struct {
		name     string
		from, to uuid.UUID
		amount   int64
		currency string
		want     error
	}{
		{"unknown source", uuid.New(), usd.ID, 10, "USD", ledger.ErrAccountNotFound},
		{"unknown destination", usd.ID, uuid.New(), 10, "USD", ledger.ErrAccountNotFound},
		{"insufficient funds", usd.ID, usd2.ID, 101, "USD", ledger.ErrInsufficientFunds},
		{"cross currency", usd.ID, eur.ID, 10, "USD", ledger.ErrCurrencyMismatch},
		{"wrong transfer currency", usd.ID, usd2.ID, 10, "EUR", ledger.ErrCurrencyMismatch},
		{"from system account", system, usd.ID, 10, "USD", ledger.ErrSystemAccount},
		{"to system account", usd.ID, system, 10, "USD", ledger.ErrSystemAccount},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := e.transfer(c.from, c.to, c.amount, c.currency, "key-"+c.name)
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}
	if e.count(t, "transfers") != transfersBefore || e.balance(t, usd.ID) != 100 || e.balance(t, usd2.ID) != 0 {
		t.Fatal("a rejected transfer changed the ledger")
	}

	// Spending the exact balance is allowed.
	if _, err := e.transfer(usd.ID, usd2.ID, 100, "USD", "exact"); err != nil {
		t.Fatalf("exact-balance transfer: %v", err)
	}
	if e.balance(t, usd.ID) != 0 || e.balance(t, usd2.ID) != 100 {
		t.Fatal("exact-balance transfer produced wrong balances")
	}
}

func TestReadsReportMissingRecords(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.svc.GetAccount(ctx, e.db.Pool, uuid.New()); !errors.Is(err, ledger.ErrAccountNotFound) {
		t.Fatalf("GetAccount: %v", err)
	}
	if _, err := e.svc.GetTransfer(ctx, e.db.Pool, uuid.New()); !errors.Is(err, ledger.ErrTransferNotFound) {
		t.Fatalf("GetTransfer: %v", err)
	}
	if _, err := e.svc.ListEntries(ctx, e.db.Pool, uuid.New(), 0, 10); !errors.Is(err, ledger.ErrAccountNotFound) {
		t.Fatalf("ListEntries: %v", err)
	}
}

func TestListEntriesPaginatesNewestFirst(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	alice := e.createAccount(t, "USD", 1000)
	bob := e.createAccount(t, "USD", 0)
	for i := 0; i < 5; i++ {
		if _, err := e.transfer(alice.ID, bob.ID, int64(10*(i+1)), "USD", "page-"+string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
	}
	// Alice has 6 entries: the funding credit and 5 debits.
	var all []ledger.Entry
	before := int64(0)
	for {
		page, err := e.svc.ListEntries(ctx, e.db.Pool, alice.ID, before, 4)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, page...)
		if len(page) < 4 {
			break
		}
		before = page[len(page)-1].ID
	}
	if len(all) != 6 {
		t.Fatalf("got %d entries, want 6", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i].ID >= all[i-1].ID {
			t.Fatal("entries are not newest first")
		}
	}
	if all[0].BalanceAfter != 1000-150 || all[len(all)-1].Amount != 1000 {
		t.Fatalf("unexpected first/last entries: %+v / %+v", all[0], all[len(all)-1])
	}
	// Each balance_after continues the previous one.
	for i := len(all) - 2; i >= 0; i-- {
		if all[i].BalanceAfter != all[i+1].BalanceAfter+all[i].Amount {
			t.Fatalf("broken running balance at entry %d", all[i].ID)
		}
	}
}

package postgres_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jsankalp715/Idempotent-Payments-Ledger-/internal/postgres"
	"github.com/jsankalp715/Idempotent-Payments-Ledger-/internal/testdb"
)

// These tests talk to the schema with raw SQL, bypassing the application, to
// show that the database rejects corrupt ledger states on its own.

type fixture struct {
	conn   *pgx.Conn
	system string // USD system account
	alice  string // funded with 1000
	bob    string // funded with 500
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db := testdb.New(t)
	conn := db.Conn(t)
	ctx := context.Background()
	f := &fixture{conn: conn}
	mustQueryRow(t, conn, `INSERT INTO accounts (currency, is_system) VALUES ('USD', true) RETURNING id`).Scan(&f.system)
	mustQueryRow(t, conn, `INSERT INTO accounts (currency) VALUES ('USD') RETURNING id`).Scan(&f.alice)
	mustQueryRow(t, conn, `INSERT INTO accounts (currency) VALUES ('USD') RETURNING id`).Scan(&f.bob)
	if err := post(ctx, conn, "funding", nil, f.system, f.alice, 1000); err != nil {
		t.Fatalf("fund alice: %v", err)
	}
	if err := post(ctx, conn, "funding", nil, f.system, f.bob, 500); err != nil {
		t.Fatalf("fund bob: %v", err)
	}
	return f
}

func mustQueryRow(t *testing.T, conn *pgx.Conn, sql string, args ...any) pgx.Row {
	t.Helper()
	return conn.QueryRow(context.Background(), sql, args...)
}

// post writes a correct double-entry transfer the way the application does:
// lock both accounts in id order, insert the transfer and two entries, update
// the cached balances, commit.
func post(ctx context.Context, conn *pgx.Conn, kind string, key *string, from, to string, amount int64) error {
	return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id::text, balance FROM accounts WHERE id = ANY($1::uuid[]) ORDER BY id FOR UPDATE`, []string{from, to})
		if err != nil {
			return err
		}
		balances := map[string]int64{}
		for rows.Next() {
			var id string
			var bal int64
			if err := rows.Scan(&id, &bal); err != nil {
				return err
			}
			balances[id] = bal
		}
		if err := rows.Err(); err != nil {
			return err
		}
		var hash []byte
		if key != nil {
			hash = make([]byte, 32)
		}
		var transferID string
		if err := tx.QueryRow(ctx,
			`INSERT INTO transfers (kind, idempotency_key, request_hash, from_account_id, to_account_id, amount, currency)
			 VALUES ($1, $2, $3, $4, $5, $6, 'USD') RETURNING id`,
			kind, key, hash, from, to, amount).Scan(&transferID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO entries (transfer_id, account_id, currency, amount, balance_after)
			 VALUES ($1, $2, 'USD', $3, $4), ($1, $5, 'USD', $6, $7)`,
			transferID, from, -amount, balances[from]-amount, to, amount, balances[to]+amount); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE accounts SET balance = balance - $2 WHERE id = $1`, from, amount); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE accounts SET balance = balance + $2 WHERE id = $1`, to, amount)
		return err
	})
}

// inTx runs fn and the COMMIT, returning the first error. Deferred
// constraint triggers report their violations at COMMIT.
func inTx(conn *pgx.Conn, fn func(tx pgx.Tx) error) error {
	return pgx.BeginFunc(context.Background(), conn, fn)
}

func requireSQLState(t *testing.T, err error, code, msgPart string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected SQLSTATE %s, got success", code)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected a PostgreSQL error with SQLSTATE %s, got %T: %v", code, err, err)
	}
	if pgErr.Code != code {
		t.Fatalf("expected SQLSTATE %s, got %s: %s", code, pgErr.Code, pgErr.Message)
	}
	if msgPart != "" && !strings.Contains(pgErr.Message+" "+pgErr.ConstraintName, msgPart) {
		t.Fatalf("error %q (constraint %q) does not mention %q", pgErr.Message, pgErr.ConstraintName, msgPart)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	db := testdb.New(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := postgres.Migrate(context.Background(), db.Pool, logger); err != nil {
		t.Fatalf("second migration run: %v", err)
	}
}

func TestValidTransferCommits(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	key := "k-1"
	if err := post(ctx, f.conn, "transfer", &key, f.alice, f.bob, 300); err != nil {
		t.Fatalf("post: %v", err)
	}
	var alice, bob, system int64
	mustQueryRow(t, f.conn, `SELECT balance FROM accounts WHERE id = $1`, f.alice).Scan(&alice)
	mustQueryRow(t, f.conn, `SELECT balance FROM accounts WHERE id = $1`, f.bob).Scan(&bob)
	mustQueryRow(t, f.conn, `SELECT balance FROM accounts WHERE id = $1`, f.system).Scan(&system)
	if alice != 700 || bob != 800 || system != -1500 {
		t.Fatalf("balances alice=%d bob=%d system=%d, want 700/800/-1500", alice, bob, system)
	}
}

func TestEntriesAndTransfersAreAppendOnly(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, stmt := range []string{
		`UPDATE entries SET amount = amount * 2`,
		`DELETE FROM entries`,
		`TRUNCATE entries CASCADE`,
		`UPDATE transfers SET amount = 1`,
		`DELETE FROM transfers`,
		`TRUNCATE transfers CASCADE`,
		`TRUNCATE accounts CASCADE`,
	} {
		_, err := f.conn.Exec(ctx, stmt)
		requireSQLState(t, err, "23000", "append-only")
	}
	var n int
	mustQueryRow(t, f.conn, `SELECT count(*) FROM entries`).Scan(&n)
	if n != 4 {
		t.Fatalf("entries count = %d after rejected mutations, want 4", n)
	}
}

func TestAccountsOnlyBalanceMayChange(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, err := f.conn.Exec(ctx, `UPDATE accounts SET currency = 'EUR' WHERE id = $1`, f.alice)
	requireSQLState(t, err, "23000", "only the balance")
	_, err = f.conn.Exec(ctx, `UPDATE accounts SET is_system = true WHERE id = $1`, f.alice)
	requireSQLState(t, err, "23000", "only the balance")
	_, err = f.conn.Exec(ctx, `DELETE FROM accounts WHERE id = $1`, f.alice)
	requireSQLState(t, err, "23000", "cannot be deleted")
}

func TestBalanceChangeWithoutEntriesIsRejected(t *testing.T) {
	f := newFixture(t)
	err := inTx(f.conn, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `UPDATE accounts SET balance = balance + 100 WHERE id = $1`, f.alice)
		return err
	})
	requireSQLState(t, err, "23514", "does not match its entries")

	err = inTx(f.conn, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `INSERT INTO accounts (currency, balance) VALUES ('USD', 50)`)
		return err
	})
	requireSQLState(t, err, "23514", "does not match its entries")
}

func TestNonSystemBalanceCannotGoNegative(t *testing.T) {
	f := newFixture(t)
	_, err := f.conn.Exec(context.Background(), `UPDATE accounts SET balance = -1 WHERE id = $1`, f.alice)
	requireSQLState(t, err, "23514", "accounts_balance_non_negative")

	// Moving more than the balance through a "correct" double entry is
	// rejected by the same constraint.
	key := "overdraft"
	err = post(context.Background(), f.conn, "transfer", &key, f.alice, f.bob, 1001)
	requireSQLState(t, err, "23514", "accounts_balance_non_negative")
}

func TestUnbalancedTransfersAreRejected(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	insertTransfer := func(tx pgx.Tx, key string, amount int64) string {
		t.Helper()
		var id string
		if err := tx.QueryRow(ctx,
			`INSERT INTO transfers (kind, idempotency_key, request_hash, from_account_id, to_account_id, amount, currency)
			 VALUES ('transfer', $1, $2, $3, $4, $5, 'USD') RETURNING id`,
			key, make([]byte, 32), f.alice, f.bob, amount).Scan(&id); err != nil {
			t.Fatalf("insert transfer: %v", err)
		}
		return id
	}

	t.Run("no entries", func(t *testing.T) {
		err := inTx(f.conn, func(tx pgx.Tx) error {
			insertTransfer(tx, "no-entries", 10)
			return nil
		})
		requireSQLState(t, err, "23514", "not a balanced double entry")
	})

	t.Run("single entry", func(t *testing.T) {
		err := inTx(f.conn, func(tx pgx.Tx) error {
			id := insertTransfer(tx, "single", 10)
			_, err := tx.Exec(ctx, `INSERT INTO entries (transfer_id, account_id, currency, amount, balance_after) VALUES ($1, $2, 'USD', -10, 990)`, id, f.alice)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE accounts SET balance = 990 WHERE id = $1`, f.alice)
			return err
		})
		requireSQLState(t, err, "23514", "not a balanced double entry")
	})

	t.Run("entries do not match the transfer", func(t *testing.T) {
		err := inTx(f.conn, func(tx pgx.Tx) error {
			id := insertTransfer(tx, "mismatch", 10)
			// Balanced between themselves (sum 0) but for 20, not 10.
			if _, err := tx.Exec(ctx, `INSERT INTO entries (transfer_id, account_id, currency, amount, balance_after) VALUES ($1, $2, 'USD', -20, 980), ($1, $3, 'USD', 20, 520)`, id, f.alice, f.bob); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE accounts SET balance = 980 WHERE id = $1`, f.alice); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE accounts SET balance = 520 WHERE id = $1`, f.bob)
			return err
		})
		requireSQLState(t, err, "23514", "not a balanced double entry")
	})

	t.Run("extra entry on an existing transfer", func(t *testing.T) {
		key := "existing"
		if err := post(ctx, f.conn, "transfer", &key, f.alice, f.bob, 10); err != nil {
			t.Fatalf("post: %v", err)
		}
		var id string
		mustQueryRow(t, f.conn, `SELECT id FROM transfers WHERE idempotency_key = $1`, key).Scan(&id)
		err := inTx(f.conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `INSERT INTO entries (transfer_id, account_id, currency, amount, balance_after) VALUES ($1, $2, 'USD', 5, -1495)`, id, f.system); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE accounts SET balance = -1495 WHERE id = $1`, f.system)
			return err
		})
		requireSQLState(t, err, "23514", "not a balanced double entry")
	})
}

func TestEntryChainMustContinuePreviousBalance(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	err := inTx(f.conn, func(tx pgx.Tx) error {
		var id string
		if err := tx.QueryRow(ctx,
			`INSERT INTO transfers (kind, idempotency_key, request_hash, from_account_id, to_account_id, amount, currency)
			 VALUES ('transfer', 'chain', $1, $2, $3, 10, 'USD') RETURNING id`,
			make([]byte, 32), f.alice, f.bob).Scan(&id); err != nil {
			return err
		}
		// Alice has 1000, so balance_after must be 990, not 5000.
		_, err := tx.Exec(ctx, `INSERT INTO entries (transfer_id, account_id, currency, amount, balance_after) VALUES ($1, $2, 'USD', -10, 5000)`, id, f.alice)
		return err
	})
	requireSQLState(t, err, "23514", "expected 1000 + -10")
}

func TestCurrenciesMustAgree(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var eur string
	mustQueryRow(t, f.conn, `INSERT INTO accounts (currency) VALUES ('EUR') RETURNING id`).Scan(&eur)
	_, err := f.conn.Exec(ctx,
		`INSERT INTO transfers (kind, idempotency_key, request_hash, from_account_id, to_account_id, amount, currency)
		 VALUES ('transfer', 'fx', $1, $2, $3, 10, 'USD')`, make([]byte, 32), f.alice, eur)
	requireSQLState(t, err, "23503", "transfers_to_account_fk")

	_, err = f.conn.Exec(ctx, `INSERT INTO accounts (currency) VALUES ('usd')`)
	requireSQLState(t, err, "23514", "accounts_currency_format")
}

func TestTransferConstraints(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	insert := `INSERT INTO transfers (kind, idempotency_key, request_hash, from_account_id, to_account_id, amount, currency)
	           VALUES ($1, $2, $3, $4, $5, $6, 'USD')`
	hash := make([]byte, 32)

	_, err := f.conn.Exec(ctx, insert, "transfer", nil, nil, f.alice, f.bob, 10)
	requireSQLState(t, err, "23514", "transfers_client_transfer_has_key")
	_, err = f.conn.Exec(ctx, insert, "transfer", "k", hash, f.alice, f.alice, 10)
	requireSQLState(t, err, "23514", "transfers_distinct_accounts")
	_, err = f.conn.Exec(ctx, insert, "transfer", "k", hash, f.alice, f.bob, 0)
	requireSQLState(t, err, "23514", "transfers_amount_positive")
	_, err = f.conn.Exec(ctx, insert, "refund", "k", hash, f.alice, f.bob, 10)
	requireSQLState(t, err, "23514", "transfers_kind_valid")

	key := "once"
	if err := post(ctx, f.conn, "transfer", &key, f.alice, f.bob, 10); err != nil {
		t.Fatalf("first post: %v", err)
	}
	err = post(ctx, f.conn, "transfer", &key, f.alice, f.bob, 10)
	requireSQLState(t, err, "23505", "transfers_idempotency_key_key")
}

func TestOneSystemAccountPerCurrency(t *testing.T) {
	f := newFixture(t)
	_, err := f.conn.Exec(context.Background(), `INSERT INTO accounts (currency, is_system) VALUES ('USD', true)`)
	requireSQLState(t, err, "23505", "accounts_one_system_account_per_currency")
}

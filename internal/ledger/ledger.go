package ledger

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jsankalp715/Idempotent-Payments-Ledger-/internal/postgres"
)

// TransferKeyConstraint is the unique constraint that makes an idempotency key
// usable for at most one transfer, ever. A violation means a concurrent request
// with the same key committed first; retrying the transaction observes it.
const TransferKeyConstraint = "transfers_idempotency_key_key"

// Service holds the ledger's business rules. It is stateless: write methods
// run in the caller's transaction, read methods on any postgres.DBTX.
type Service struct{}

// NewService returns a ledger Service.
func NewService() *Service { return &Service{} }

const accountColumns = `id, currency, is_system, balance, created_at, updated_at`

func scanAccount(row pgx.Row) (Account, error) {
	var a Account
	var system bool
	if err := row.Scan(&a.ID, &a.Currency, &system, &a.Balance, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return Account{}, err
	}
	a.Kind = AccountUser
	if system {
		a.Kind = AccountSystem
	}
	return a, nil
}

// CreateAccount inserts a user account and, if requested, funds it from the
// currency's system account with a funding transfer in the same transaction.
func (s *Service) CreateAccount(ctx context.Context, tx pgx.Tx, in CreateAccountInput) (Account, error) {
	if err := in.Validate(); err != nil {
		return Account{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Account{}, fmt.Errorf("generate account id: %w", err)
	}
	acc, err := scanAccount(tx.QueryRow(ctx,
		`INSERT INTO accounts (id, currency) VALUES ($1, $2) RETURNING `+accountColumns, id, in.Currency))
	if err != nil {
		return Account{}, fmt.Errorf("insert account: %w", err)
	}
	if in.InitialBalance == 0 {
		return acc, nil
	}

	systemID, err := s.ensureSystemAccount(ctx, tx, in.Currency)
	if err != nil {
		return Account{}, err
	}
	locked, err := lockAccounts(ctx, tx, systemID, acc.ID)
	if err != nil {
		return Account{}, err
	}
	if _, err := s.post(ctx, tx, KindFunding, nil, nil, locked[systemID], locked[acc.ID], in.InitialBalance); err != nil {
		return Account{}, err
	}
	return s.GetAccount(ctx, tx, acc.ID)
}

// ensureSystemAccount returns the id of the currency's system account,
// creating it on first use. Concurrent creators converge on one row through
// the partial unique index.
func (s *Service) ensureSystemAccount(ctx context.Context, tx pgx.Tx, currency string) (uuid.UUID, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, fmt.Errorf("generate account id: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO accounts (id, currency, is_system) VALUES ($1, $2, true)
		 ON CONFLICT (currency) WHERE is_system DO NOTHING`, id, currency); err != nil {
		return uuid.Nil, fmt.Errorf("ensure system account: %w", err)
	}
	var systemID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM accounts WHERE currency = $1 AND is_system`, currency).Scan(&systemID); err != nil {
		return uuid.Nil, fmt.Errorf("read system account: %w", err)
	}
	return systemID, nil
}

// Transfer executes a client transfer inside tx. Both accounts are locked
// with SELECT ... FOR UPDATE in ascending id order, so two transfers touching
// the same pair of accounts (in either direction) always queue in the same
// order and cannot deadlock. The balance check happens under that lock; the
// CHECK constraint on accounts is the second line of defence.
//
// key and requestHash are stored on the transfer row, whose unique constraint
// guarantees the key moves money at most once even after the idempotency
// record expires.
func (s *Service) Transfer(ctx context.Context, tx pgx.Tx, in TransferInput, key string, requestHash []byte) (Transfer, error) {
	if err := in.Validate(); err != nil {
		return Transfer{}, err
	}
	locked, err := lockAccounts(ctx, tx, in.FromAccountID, in.ToAccountID)
	if err != nil {
		return Transfer{}, err
	}
	from, ok := locked[in.FromAccountID]
	if !ok {
		return Transfer{}, newError(ErrAccountNotFound, "from_account %s does not exist", in.FromAccountID)
	}
	to, ok := locked[in.ToAccountID]
	if !ok {
		return Transfer{}, newError(ErrAccountNotFound, "to_account %s does not exist", in.ToAccountID)
	}
	if from.Kind == AccountSystem || to.Kind == AccountSystem {
		return Transfer{}, newError(ErrSystemAccount, "system accounts cannot take part in client transfers")
	}
	if from.Currency != in.Currency || to.Currency != in.Currency {
		return Transfer{}, newError(ErrCurrencyMismatch,
			"transfer currency %s does not match from_account (%s) and to_account (%s)", in.Currency, from.Currency, to.Currency)
	}
	if from.Balance < in.Amount {
		return Transfer{}, newError(ErrInsufficientFunds, "from_account %s has insufficient funds for a transfer of %d", from.ID, in.Amount)
	}
	return s.post(ctx, tx, KindTransfer, &key, requestHash, from, to, in.Amount)
}

// lockAccounts locks the given accounts FOR UPDATE and returns those that
// exist. ORDER BY is applied before row locks are taken, so locks are
// acquired in ascending id order whatever order the ids were passed in.
func lockAccounts(ctx context.Context, tx pgx.Tx, a, b uuid.UUID) (map[uuid.UUID]Account, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+accountColumns+` FROM accounts WHERE id IN ($1, $2) ORDER BY id FOR UPDATE`, a, b)
	if err != nil {
		return nil, fmt.Errorf("lock accounts: %w", err)
	}
	defer rows.Close()
	out := make(map[uuid.UUID]Account, 2)
	for rows.Next() {
		acc, err := scanAccount(rows)
		if err != nil {
			return nil, fmt.Errorf("scan account: %w", err)
		}
		out[acc.ID] = acc
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lock accounts: %w", err)
	}
	return out, nil
}

// post writes one transfer and its two entries and moves the cached balances.
// The caller must hold FOR UPDATE locks on both accounts, which are passed in
// with the balances read under those locks.
func (s *Service) post(ctx context.Context, tx pgx.Tx, kind TransferKind, key *string, requestHash []byte,
	from, to Account, amount int64) (Transfer, error) {
	if to.Balance > math.MaxInt64-amount || from.Balance < math.MinInt64+amount {
		return Transfer{}, newError(ErrBalanceOverflow, "transfer of %d would overflow an account balance", amount)
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Transfer{}, fmt.Errorf("generate transfer id: %w", err)
	}
	t := Transfer{
		ID:            id,
		Kind:          kind,
		FromAccountID: from.ID,
		ToAccountID:   to.ID,
		Amount:        amount,
		Currency:      from.Currency,
		Entries: []Entry{
			{TransferID: id, AccountID: from.ID, Currency: from.Currency, Amount: -amount, BalanceAfter: from.Balance - amount},
			{TransferID: id, AccountID: to.ID, Currency: to.Currency, Amount: amount, BalanceAfter: to.Balance + amount},
		},
	}

	// One round trip for all writes keeps the account locks short.
	batch := &pgx.Batch{}
	batch.Queue(`INSERT INTO transfers (id, kind, idempotency_key, request_hash, from_account_id, to_account_id, amount, currency)
	             VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING created_at`,
		t.ID, string(kind), key, requestHash, t.FromAccountID, t.ToAccountID, amount, t.Currency).
		QueryRow(func(row pgx.Row) error { return row.Scan(&t.CreatedAt) })
	for i := range t.Entries {
		e := &t.Entries[i]
		batch.Queue(`INSERT INTO entries (transfer_id, account_id, currency, amount, balance_after)
		             VALUES ($1, $2, $3, $4, $5) RETURNING id, created_at`,
			e.TransferID, e.AccountID, e.Currency, e.Amount, e.BalanceAfter).
			QueryRow(func(row pgx.Row) error { return row.Scan(&e.ID, &e.CreatedAt) })
	}
	for i := range t.Entries {
		e := t.Entries[i]
		batch.Queue(`UPDATE accounts SET balance = balance + $2 WHERE id = $1 RETURNING balance`, e.AccountID, e.Amount).
			QueryRow(func(row pgx.Row) error {
				var balance int64
				if err := row.Scan(&balance); err != nil {
					return err
				}
				// The row lock makes this impossible; checking it costs nothing.
				if balance != e.BalanceAfter {
					return fmt.Errorf("account %s balance moved to %d, expected %d: row lock not held", e.AccountID, balance, e.BalanceAfter)
				}
				return nil
			})
	}
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return Transfer{}, fmt.Errorf("write transfer: %w", err)
	}
	return t, nil
}

// TransferByKey returns the transfer created with idempotency key key, with
// the request hash stored alongside it. found is false if there is none.
func (s *Service) TransferByKey(ctx context.Context, db postgres.DBTX, key string) (t Transfer, requestHash []byte, found bool, err error) {
	var id uuid.UUID
	err = db.QueryRow(ctx, `SELECT id, request_hash FROM transfers WHERE idempotency_key = $1`, key).Scan(&id, &requestHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return Transfer{}, nil, false, nil
	}
	if err != nil {
		return Transfer{}, nil, false, fmt.Errorf("find transfer by key: %w", err)
	}
	t, err = s.GetTransfer(ctx, db, id)
	if err != nil {
		return Transfer{}, nil, false, err
	}
	return t, requestHash, true, nil
}

// GetAccount returns the account with the given id.
func (s *Service) GetAccount(ctx context.Context, db postgres.DBTX, id uuid.UUID) (Account, error) {
	acc, err := scanAccount(db.QueryRow(ctx, `SELECT `+accountColumns+` FROM accounts WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, newError(ErrAccountNotFound, "account %s does not exist", id)
	}
	if err != nil {
		return Account{}, fmt.Errorf("get account: %w", err)
	}
	return acc, nil
}

const entryColumns = `id, transfer_id, account_id, currency, amount, balance_after, created_at`

func scanEntries(rows pgx.Rows) ([]Entry, error) {
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.TransferID, &e.AccountID, &e.Currency, &e.Amount, &e.BalanceAfter, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan entry: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read entries: %w", err)
	}
	return out, nil
}

// GetTransfer returns a transfer with its two entries (debit first).
func (s *Service) GetTransfer(ctx context.Context, db postgres.DBTX, id uuid.UUID) (Transfer, error) {
	var t Transfer
	var kind string
	err := db.QueryRow(ctx,
		`SELECT id, kind, from_account_id, to_account_id, amount, currency, created_at FROM transfers WHERE id = $1`, id).
		Scan(&t.ID, &kind, &t.FromAccountID, &t.ToAccountID, &t.Amount, &t.Currency, &t.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Transfer{}, newError(ErrTransferNotFound, "transfer %s does not exist", id)
	}
	if err != nil {
		return Transfer{}, fmt.Errorf("get transfer: %w", err)
	}
	t.Kind = TransferKind(kind)
	rows, err := db.Query(ctx, `SELECT `+entryColumns+` FROM entries WHERE transfer_id = $1 ORDER BY amount`, id)
	if err != nil {
		return Transfer{}, fmt.Errorf("get transfer entries: %w", err)
	}
	if t.Entries, err = scanEntries(rows); err != nil {
		return Transfer{}, err
	}
	return t, nil
}

// ListEntries returns up to limit entries of an account, newest first. If
// beforeID is positive only entries with a smaller id are returned, which
// makes beforeID a stable pagination cursor.
func (s *Service) ListEntries(ctx context.Context, db postgres.DBTX, accountID uuid.UUID, beforeID int64, limit int) ([]Entry, error) {
	var exists bool
	if err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM accounts WHERE id = $1)`, accountID).Scan(&exists); err != nil {
		return nil, fmt.Errorf("check account: %w", err)
	}
	if !exists {
		return nil, newError(ErrAccountNotFound, "account %s does not exist", accountID)
	}
	if beforeID <= 0 {
		beforeID = math.MaxInt64
	}
	rows, err := db.Query(ctx,
		`SELECT `+entryColumns+` FROM entries WHERE account_id = $1 AND id < $2 ORDER BY id DESC LIMIT $3`,
		accountID, beforeID, limit)
	if err != nil {
		return nil, fmt.Errorf("list entries: %w", err)
	}
	return scanEntries(rows)
}

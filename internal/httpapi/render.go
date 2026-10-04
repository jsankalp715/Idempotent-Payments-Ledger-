package httpapi

import (
	"time"

	"github.com/jsankalp715/Idempotent-Payments-Ledger-/internal/ledger"
)

// JSON representations. They contain only immutable data or data captured at
// response time, and timestamps are rendered in UTC with a fixed format, so a
// response rebuilt later from the same rows is byte-identical.

type accountJSON struct {
	ID        string `json:"id"`
	Currency  string `json:"currency"`
	Type      string `json:"type"`
	Balance   int64  `json:"balance"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type transferJSON struct {
	ID          string              `json:"id"`
	Kind        string              `json:"kind"`
	FromAccount string              `json:"from_account"`
	ToAccount   string              `json:"to_account"`
	Amount      int64               `json:"amount"`
	Currency    string              `json:"currency"`
	CreatedAt   string              `json:"created_at"`
	Entries     []transferEntryJSON `json:"entries"`
}

// transferEntryJSON omits balance_after: a transfer response is seen by the
// sender and must not reveal the recipient's balance.
type transferEntryJSON struct {
	ID        int64  `json:"id"`
	AccountID string `json:"account_id"`
	Direction string `json:"direction"`
	Amount    int64  `json:"amount"`
}

type entryJSON struct {
	ID           int64  `json:"id"`
	TransferID   string `json:"transfer_id"`
	AccountID    string `json:"account_id"`
	Direction    string `json:"direction"`
	Amount       int64  `json:"amount"`
	Currency     string `json:"currency"`
	BalanceAfter int64  `json:"balance_after"`
	CreatedAt    string `json:"created_at"`
}

type entryListJSON struct {
	Entries    []entryJSON `json:"entries"`
	NextCursor *string     `json:"next_cursor"`
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func toAccountJSON(a ledger.Account) accountJSON {
	return accountJSON{
		ID:        a.ID.String(),
		Currency:  a.Currency,
		Type:      string(a.Kind),
		Balance:   a.Balance,
		CreatedAt: formatTime(a.CreatedAt),
		UpdatedAt: formatTime(a.UpdatedAt),
	}
}

func toTransferJSON(t ledger.Transfer) transferJSON {
	out := transferJSON{
		ID:          t.ID.String(),
		Kind:        string(t.Kind),
		FromAccount: t.FromAccountID.String(),
		ToAccount:   t.ToAccountID.String(),
		Amount:      t.Amount,
		Currency:    t.Currency,
		CreatedAt:   formatTime(t.CreatedAt),
		Entries:     make([]transferEntryJSON, len(t.Entries)),
	}
	for i, e := range t.Entries {
		out.Entries[i] = transferEntryJSON{ID: e.ID, AccountID: e.AccountID.String(), Direction: e.Direction(), Amount: e.Amount}
	}
	return out
}

func toEntryJSON(e ledger.Entry) entryJSON {
	return entryJSON{
		ID:           e.ID,
		TransferID:   e.TransferID.String(),
		AccountID:    e.AccountID.String(),
		Direction:    e.Direction(),
		Amount:       e.Amount,
		Currency:     e.Currency,
		BalanceAfter: e.BalanceAfter,
		CreatedAt:    formatTime(e.CreatedAt),
	}
}

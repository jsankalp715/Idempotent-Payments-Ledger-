// Package ledger implements the double-entry ledger: accounts, transfers and
// their immutable entries. Write operations run inside a transaction supplied
// by the caller, so they compose with the idempotency layer, which must commit
// its key record in the same transaction as the money movement.
package ledger

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// MaxAmount caps transfer amounts and initial balances, in minor units.
// 10^15 is below 2^53, so amounts survive JSON parsers that use float64.
const MaxAmount int64 = 1_000_000_000_000_000

// AccountKind distinguishes customer accounts from the per-currency system
// account through which money enters the ledger.
type AccountKind string

const (
	AccountUser   AccountKind = "user"
	AccountSystem AccountKind = "system"
)

// Account is a ledger account. Balance is a cache of the sum of the account's
// entries, maintained in the same transaction as every entry.
type Account struct {
	ID        uuid.UUID
	Currency  string
	Kind      AccountKind
	Balance   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// TransferKind tells client transfers apart from initial-balance funding.
type TransferKind string

const (
	KindTransfer TransferKind = "transfer"
	KindFunding  TransferKind = "funding"
)

// Transfer moves Amount from one account to another. It is recorded as
// exactly two entries: a debit of -Amount and a credit of +Amount.
type Transfer struct {
	ID            uuid.UUID
	Kind          TransferKind
	FromAccountID uuid.UUID
	ToAccountID   uuid.UUID
	Amount        int64
	Currency      string
	CreatedAt     time.Time
	// Entries holds the debit followed by the credit.
	Entries []Entry
}

// Entry is one immutable leg of a transfer. Amount is signed: negative for a
// debit (money leaving the account), positive for a credit.
type Entry struct {
	ID           int64
	TransferID   uuid.UUID
	AccountID    uuid.UUID
	Currency     string
	Amount       int64
	BalanceAfter int64
	CreatedAt    time.Time
}

// Direction is "debit" for outgoing money and "credit" for incoming money.
func (e Entry) Direction() string {
	if e.Amount < 0 {
		return "debit"
	}
	return "credit"
}

// CreateAccountInput describes a new account. A positive InitialBalance is
// moved in from the currency's system account by a funding transfer.
type CreateAccountInput struct {
	Currency       string
	InitialBalance int64
}

// TransferInput describes a client transfer.
type TransferInput struct {
	FromAccountID uuid.UUID
	ToAccountID   uuid.UUID
	Amount        int64
	Currency      string
}

var currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)

// Validate checks the request in isolation (no database access).
func (in CreateAccountInput) Validate() error {
	var v ValidationError
	validateCurrency(&v, "currency", in.Currency)
	switch {
	case in.InitialBalance < 0:
		v.Add("initial_balance", "must not be negative")
	case in.InitialBalance > MaxAmount:
		v.Add("initial_balance", fmt.Sprintf("must not exceed %d", MaxAmount))
	}
	return v.Err()
}

// Validate checks the request in isolation (no database access).
func (in TransferInput) Validate() error {
	var v ValidationError
	if in.FromAccountID == uuid.Nil {
		v.Add("from_account", "is required")
	}
	if in.ToAccountID == uuid.Nil {
		v.Add("to_account", "is required")
	}
	if in.FromAccountID != uuid.Nil && in.FromAccountID == in.ToAccountID {
		v.Add("to_account", "must differ from from_account")
	}
	ValidateAmount(&v, "amount", in.Amount)
	validateCurrency(&v, "currency", in.Currency)
	return v.Err()
}

// ValidateAmount records a problem unless 0 < amount <= MaxAmount.
func ValidateAmount(v *ValidationError, field string, amount int64) {
	switch {
	case amount <= 0:
		v.Add(field, "must be a positive integer number of minor units")
	case amount > MaxAmount:
		v.Add(field, fmt.Sprintf("must not exceed %d", MaxAmount))
	}
}

func validateCurrency(v *ValidationError, field, currency string) {
	if !currencyPattern.MatchString(currency) {
		v.Add(field, "must be a three-letter upper-case ISO 4217 code such as USD")
	}
}

// FieldError is a problem with a single request field.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// ValidationError collects every problem found in a request.
type ValidationError struct {
	Fields []FieldError
}

// Add records a problem with field.
func (v *ValidationError) Add(field, message string) {
	v.Fields = append(v.Fields, FieldError{Field: field, Message: message})
}

// Err returns v as an error if it holds any problem, nil otherwise.
func (v *ValidationError) Err() error {
	if len(v.Fields) == 0 {
		return nil
	}
	return v
}

func (v *ValidationError) Error() string {
	parts := make([]string, len(v.Fields))
	for i, f := range v.Fields {
		parts[i] = f.Field + " " + f.Message
	}
	return "invalid request: " + strings.Join(parts, "; ")
}

// Error is a business-rule outcome with a stable machine-readable code. These
// are definitive results of executing a request (not transient failures), so
// the idempotency layer stores and replays them.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

// Is matches errors by code, so errors.Is(err, ErrInsufficientFunds) holds
// for every insufficient-funds error whatever its message.
func (e *Error) Is(target error) bool {
	var t *Error
	return errors.As(target, &t) && t.Code == e.Code
}

// Sentinels for errors.Is; the errors actually returned carry specific messages.
var (
	ErrAccountNotFound   = &Error{Code: "account_not_found", Message: "account not found"}
	ErrTransferNotFound  = &Error{Code: "transfer_not_found", Message: "transfer not found"}
	ErrInsufficientFunds = &Error{Code: "insufficient_funds", Message: "insufficient funds"}
	ErrCurrencyMismatch  = &Error{Code: "currency_mismatch", Message: "currency mismatch"}
	ErrSystemAccount     = &Error{Code: "system_account_not_allowed", Message: "system accounts cannot take part in client transfers"}
	ErrBalanceOverflow   = &Error{Code: "balance_overflow", Message: "resulting balance is out of range"}
)

func newError(kind *Error, format string, args ...any) *Error {
	return &Error{Code: kind.Code, Message: fmt.Sprintf(format, args...)}
}

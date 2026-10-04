package ledger

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func fields(err error) []string {
	var v *ValidationError
	if !errors.As(err, &v) {
		return nil
	}
	out := make([]string, len(v.Fields))
	for i, f := range v.Fields {
		out[i] = f.Field
	}
	return out
}

func TestTransferInputValidate(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	valid := TransferInput{FromAccountID: a, ToAccountID: b, Amount: 1, Currency: "USD"}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid input rejected: %v", err)
	}
	maxed := valid
	maxed.Amount = MaxAmount
	if err := maxed.Validate(); err != nil {
		t.Fatalf("MaxAmount rejected: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*TransferInput)
		fields []string
	}{
		{"zero amount", func(in *TransferInput) { in.Amount = 0 }, []string{"amount"}},
		{"negative amount", func(in *TransferInput) { in.Amount = -5 }, []string{"amount"}},
		{"amount above max", func(in *TransferInput) { in.Amount = MaxAmount + 1 }, []string{"amount"}},
		{"lower-case currency", func(in *TransferInput) { in.Currency = "usd" }, []string{"currency"}},
		{"long currency", func(in *TransferInput) { in.Currency = "USDT" }, []string{"currency"}},
		{"empty currency", func(in *TransferInput) { in.Currency = "" }, []string{"currency"}},
		{"missing from", func(in *TransferInput) { in.FromAccountID = uuid.Nil }, []string{"from_account"}},
		{"missing to", func(in *TransferInput) { in.ToAccountID = uuid.Nil }, []string{"to_account"}},
		{"same account", func(in *TransferInput) { in.ToAccountID = in.FromAccountID }, []string{"to_account"}},
		{"everything wrong", func(in *TransferInput) {
			*in = TransferInput{Amount: -1, Currency: "x"}
		}, []string{"from_account", "to_account", "amount", "currency"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := valid
			c.mutate(&in)
			err := in.Validate()
			if got := fields(err); strings.Join(got, ",") != strings.Join(c.fields, ",") {
				t.Fatalf("problem fields = %v, want %v (err: %v)", got, c.fields, err)
			}
		})
	}
}

func TestCreateAccountInputValidate(t *testing.T) {
	for _, in := range []CreateAccountInput{
		{Currency: "USD"},
		{Currency: "EUR", InitialBalance: 1},
		{Currency: "JPY", InitialBalance: MaxAmount},
	} {
		if err := in.Validate(); err != nil {
			t.Errorf("%+v rejected: %v", in, err)
		}
	}
	cases := []struct {
		in     CreateAccountInput
		fields []string
	}{
		{CreateAccountInput{Currency: "us"}, []string{"currency"}},
		{CreateAccountInput{Currency: "USD", InitialBalance: -1}, []string{"initial_balance"}},
		{CreateAccountInput{Currency: "USD", InitialBalance: MaxAmount + 1}, []string{"initial_balance"}},
		{CreateAccountInput{Currency: "", InitialBalance: -1}, []string{"currency", "initial_balance"}},
	}
	for _, c := range cases {
		if got := fields(c.in.Validate()); strings.Join(got, ",") != strings.Join(c.fields, ",") {
			t.Errorf("%+v: problem fields = %v, want %v", c.in, got, c.fields)
		}
	}
}

func TestValidationErrorMessage(t *testing.T) {
	var v ValidationError
	if v.Err() != nil {
		t.Fatal("empty ValidationError must be nil")
	}
	v.Add("amount", "must be positive")
	v.Add("currency", "is invalid")
	want := "invalid request: amount must be positive; currency is invalid"
	if got := v.Err().Error(); got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

func TestErrorsMatchByCode(t *testing.T) {
	err := newError(ErrInsufficientFunds, "account x cannot pay 10")
	if !errors.Is(err, ErrInsufficientFunds) {
		t.Fatal("specific error does not match its sentinel")
	}
	if errors.Is(err, ErrAccountNotFound) {
		t.Fatal("error matches a different sentinel")
	}
	if err.Error() != "account x cannot pay 10" || err.Code != "insufficient_funds" {
		t.Fatalf("unexpected error %q / %q", err.Error(), err.Code)
	}
}

func TestEntryDirection(t *testing.T) {
	if (Entry{Amount: -1}).Direction() != "debit" || (Entry{Amount: 1}).Direction() != "credit" {
		t.Fatal("wrong direction")
	}
}

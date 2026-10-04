package integration_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/jsankalp715/Idempotent-Payments-Ledger-/internal/ledgertest"
)

func TestTransferValidation(t *testing.T) {
	p := newPair(t)
	from, to := p.from.ID, p.to.ID
	cases := []struct {
		name        string
		body        string
		contentType string
		status      int
		code        string
		fields      string // comma-separated fields expected in the error
	}{
		{"empty body", ``, "", 400, "malformed_json", ""},
		{"not json", `amount=5`, "", 400, "malformed_json", ""},
		{"truncated json", `{"amount": 5`, "", 400, "malformed_json", ""},
		{"array", `[]`, "", 400, "malformed_json", ""},
		{"two objects", fmt.Sprintf(`{"from_account":%q,"to_account":%q,"amount":5,"currency":"USD"} {}`, from, to), "", 400, "malformed_json", ""},
		{"unknown field", fmt.Sprintf(`{"from_account":%q,"to_account":%q,"amount":5,"currency":"USD","memo":"x"}`, from, to), "", 400, "validation_failed", "memo"},
		{"all missing", `{}`, "", 400, "validation_failed", "from_account,to_account,amount,currency"},
		{"nulls", `{"from_account":null,"to_account":null,"amount":null,"currency":null}`, "", 400, "validation_failed", "from_account,to_account,amount,currency"},
		{"bad uuids", `{"from_account":"abc","to_account":"{` + to + `}","amount":5,"currency":"USD"}`, "", 400, "validation_failed", "from_account,to_account"},
		{"same account", fmt.Sprintf(`{"from_account":%q,"to_account":%q,"amount":5,"currency":"USD"}`, from, from), "", 400, "validation_failed", "to_account"},
		{"zero amount", fmt.Sprintf(`{"from_account":%q,"to_account":%q,"amount":0,"currency":"USD"}`, from, to), "", 400, "validation_failed", "amount"},
		{"negative amount", fmt.Sprintf(`{"from_account":%q,"to_account":%q,"amount":-1,"currency":"USD"}`, from, to), "", 400, "validation_failed", "amount"},
		{"fractional amount", fmt.Sprintf(`{"from_account":%q,"to_account":%q,"amount":1.5,"currency":"USD"}`, from, to), "", 400, "validation_failed", "amount"},
		{"string amount", fmt.Sprintf(`{"from_account":%q,"to_account":%q,"amount":"5","currency":"USD"}`, from, to), "", 400, "validation_failed", "amount"},
		{"huge amount", fmt.Sprintf(`{"from_account":%q,"to_account":%q,"amount":1000000000000001,"currency":"USD"}`, from, to), "", 400, "validation_failed", "amount"},
		{"int64 overflow", fmt.Sprintf(`{"from_account":%q,"to_account":%q,"amount":99999999999999999999,"currency":"USD"}`, from, to), "", 400, "validation_failed", "amount"},
		{"lower-case currency", fmt.Sprintf(`{"from_account":%q,"to_account":%q,"amount":5,"currency":"usd"}`, from, to), "", 400, "validation_failed", "currency"},
		{"form content type", fmt.Sprintf(`{"from_account":%q,"to_account":%q,"amount":5,"currency":"USD"}`, from, to), "application/x-www-form-urlencoded", 415, "unsupported_media_type", ""},
		{"oversized body", `{"currency":"` + strings.Repeat("A", 70_000) + `"}`, "", 413, "body_too_large", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, p.s.HTTP.URL+"/v1/transfers", strings.NewReader(c.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Idempotency-Key", newKey(t))
			ct := c.contentType
			if ct == "" {
				ct = "application/json; charset=utf-8"
			}
			req.Header.Set("Content-Type", ct)
			httpResp, err := p.s.HTTP.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer httpResp.Body.Close()
			var buf bytes.Buffer
			_, _ = buf.ReadFrom(httpResp.Body)
			resp := ledgertest.Response{Status: httpResp.StatusCode, Header: httpResp.Header, Body: buf.Bytes()}
			if resp.Status != c.status || resp.ErrorCode() != c.code {
				t.Fatalf("got %s, want %d %s", resp, c.status, c.code)
			}
			if c.fields != "" {
				var env struct {
					Error struct {
						Fields []struct {
							Field string `json:"field"`
						} `json:"fields"`
					} `json:"error"`
				}
				if err := resp.Decode(&env); err != nil {
					t.Fatal(err)
				}
				var got []string
				for _, f := range env.Error.Fields {
					got = append(got, f.Field)
				}
				if strings.Join(got, ",") != c.fields {
					t.Fatalf("error fields %v, want %s", got, c.fields)
				}
			}
		})
	}
	if n := count(t, p.s, `SELECT count(*) FROM transfers WHERE kind = 'transfer'`); n != 0 {
		t.Fatalf("%d transfers executed from invalid requests", n)
	}
	if n := count(t, p.s, `SELECT count(*) FROM idempotency_keys`); n != 0 {
		t.Fatalf("%d keys stored for invalid requests (validation errors must not be cached)", n)
	}
}

func TestTransferBusinessRules(t *testing.T) {
	s := ledgertest.NewServer(t)
	usd := s.Client.CreateAccount(t, "USD", 1_000)
	usd2 := s.Client.CreateAccount(t, "USD", 0)
	eur := s.Client.CreateAccount(t, "EUR", 1_000)
	var system string
	if err := s.DB.Pool.QueryRow(context.Background(), `SELECT id::text FROM accounts WHERE is_system AND currency = 'USD'`).Scan(&system); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		req  ledgertest.TransferRequest
		code string
	}{
		{"unknown source", ledgertest.TransferRequest{FromAccount: "00000000-0000-4000-8000-000000000000", ToAccount: usd.ID, Amount: 1, Currency: "USD"}, "account_not_found"},
		{"unknown destination", ledgertest.TransferRequest{FromAccount: usd.ID, ToAccount: "00000000-0000-4000-8000-000000000000", Amount: 1, Currency: "USD"}, "account_not_found"},
		{"cross currency", ledgertest.TransferRequest{FromAccount: usd.ID, ToAccount: eur.ID, Amount: 1, Currency: "USD"}, "currency_mismatch"},
		{"insufficient funds", ledgertest.TransferRequest{FromAccount: usd.ID, ToAccount: usd2.ID, Amount: 1_001, Currency: "USD"}, "insufficient_funds"},
		{"mint from system account", ledgertest.TransferRequest{FromAccount: system, ToAccount: usd.ID, Amount: 1, Currency: "USD"}, "system_account_not_allowed"},
		{"pay into system account", ledgertest.TransferRequest{FromAccount: usd.ID, ToAccount: system, Amount: 1, Currency: "USD"}, "system_account_not_allowed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := s.Client.MustTransfer(t, newKey(t), c.req)
			if resp.Status != http.StatusUnprocessableEntity || resp.ErrorCode() != c.code {
				t.Fatalf("got %s, want 422 %s", resp, c.code)
			}
		})
	}
	if b := s.Client.Balance(t, usd.ID); b != 1_000 {
		t.Fatalf("balance %d after rejected transfers, want 1000", b)
	}
	requireInvariants(t, s)
}

func TestAccountEndpoints(t *testing.T) {
	s := ledgertest.NewServer(t)
	ctx := context.Background()

	acc := s.Client.CreateAccount(t, "GBP", 0)
	if acc.Balance != 0 || acc.Type != "user" || acc.Currency != "GBP" {
		t.Fatalf("unexpected account %+v", acc)
	}
	resp, _ := s.Client.Do(ctx, http.MethodGet, "/v1/accounts/"+acc.ID, nil)
	var got ledgertest.Account
	if err := resp.Decode(&got); err != nil || got != acc {
		t.Fatalf("GET account: %v %+v vs %+v", err, got, acc)
	}

	for name, tc := range map[string]struct {
		body   any
		status int
		code   string
	}{
		"missing currency":   {map[string]any{"initial_balance": 5}, 400, "validation_failed"},
		"bad currency":       {map[string]any{"currency": "pounds"}, 400, "validation_failed"},
		"negative balance":   {map[string]any{"currency": "GBP", "initial_balance": -5}, 400, "validation_failed"},
		"unknown field":      {map[string]any{"currency": "GBP", "owner": "me"}, 400, "validation_failed"},
		"wrong balance type": {map[string]any{"currency": "GBP", "initial_balance": "5"}, 400, "validation_failed"},
	} {
		r, err := s.Client.Do(ctx, http.MethodPost, "/v1/accounts", tc.body)
		if err != nil || r.Status != tc.status || r.ErrorCode() != tc.code {
			t.Errorf("%s: %v %s, want %d %s", name, err, r, tc.status, tc.code)
		}
	}
	if r, _ := s.Client.Do(ctx, http.MethodPost, "/v1/accounts", map[string]any{"currency": "GBP"}, "Idempotency-Key", "bad key"); r.Status != 400 {
		t.Errorf("invalid optional key accepted: %s", r)
	}

	for path, want := range map[string]struct {
		status int
		code   string
	}{
		"/v1/accounts/00000000-0000-4000-8000-000000000000":         {404, "account_not_found"},
		"/v1/accounts/not-a-uuid":                                   {400, "invalid_id"},
		"/v1/accounts/00000000-0000-4000-8000-000000000000/entries": {404, "account_not_found"},
		"/v1/transfers/00000000-0000-4000-8000-000000000000":        {404, "transfer_not_found"},
		"/v1/transfers/nope":                                        {400, "invalid_id"},
		"/v1/accounts/" + acc.ID + "/entries?limit=0":               {400, "invalid_query"},
		"/v1/accounts/" + acc.ID + "/entries?limit=201":             {400, "invalid_query"},
		"/v1/accounts/" + acc.ID + "/entries?cursor=-4":             {400, "invalid_query"},
		"/v1/accounts/" + acc.ID + "/entries?cursor=abc":            {400, "invalid_query"},
		"/v2/accounts": {404, "not_found"},
		"/v1/accounts/00000000-0000-4000-8000-000000000000/entries/extra/segments": {404, "not_found"},
	} {
		r, err := s.Client.Do(ctx, http.MethodGet, path, nil)
		if err != nil || r.Status != want.status || r.ErrorCode() != want.code {
			t.Errorf("GET %s: %v %s, want %d %s", path, err, r, want.status, want.code)
		}
	}
	r, _ := s.Client.Do(ctx, http.MethodDelete, "/v1/accounts/"+acc.ID, nil)
	if r.Status != http.StatusMethodNotAllowed || r.ErrorCode() != "method_not_allowed" || r.Header.Get("Allow") == "" {
		t.Errorf("DELETE account: %s (Allow %q)", r, r.Header.Get("Allow"))
	}
}

func TestEntriesPagination(t *testing.T) {
	s := ledgertest.NewServer(t)
	ctx := context.Background()
	a := s.Client.CreateAccount(t, "USD", 100_000)
	b := s.Client.CreateAccount(t, "USD", 0)
	for i := 1; i <= 7; i++ {
		if r := s.Client.MustTransfer(t, newKey(t), ledgertest.TransferRequest{FromAccount: a.ID, ToAccount: b.ID, Amount: int64(i * 100), Currency: "USD"}); r.Status != 201 {
			t.Fatalf("transfer %d: %s", i, r)
		}
	}
	type entry struct {
		ID           int64  `json:"id"`
		TransferID   string `json:"transfer_id"`
		Direction    string `json:"direction"`
		Amount       int64  `json:"amount"`
		BalanceAfter int64  `json:"balance_after"`
	}
	var all []entry
	cursor := ""
	pages := 0
	for {
		path := "/v1/accounts/" + a.ID + "/entries?limit=3"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		r, err := s.Client.Do(ctx, http.MethodGet, path, nil)
		if err != nil || r.Status != 200 {
			t.Fatalf("page: %v %s", err, r)
		}
		var page struct {
			Entries    []entry `json:"entries"`
			NextCursor *string `json:"next_cursor"`
		}
		if err := r.Decode(&page); err != nil {
			t.Fatal(err)
		}
		pages++
		all = append(all, page.Entries...)
		if page.NextCursor == nil {
			break
		}
		cursor = *page.NextCursor
	}
	// 8 entries (1 funding credit + 7 debits) in pages of 3: 3 + 3 + 2.
	if len(all) != 8 || pages != 3 {
		t.Fatalf("got %d entries in %d pages, want 8 in 3", len(all), pages)
	}
	if all[0].BalanceAfter != 100_000-2_800 || all[0].Direction != "debit" || all[7].Direction != "credit" || all[7].Amount != 100_000 {
		t.Fatalf("unexpected entries: first %+v last %+v", all[0], all[7])
	}
	for i := 1; i < len(all); i++ {
		if all[i].ID >= all[i-1].ID {
			t.Fatal("entries not newest first")
		}
	}

	// GET /v1/transfers/{id} returns exactly what POST returned.
	key := newKey(t)
	posted := s.Client.MustTransfer(t, key, ledgertest.TransferRequest{FromAccount: a.ID, ToAccount: b.ID, Amount: 1, Currency: "USD"})
	var tr ledgertest.Transfer
	if err := posted.Decode(&tr); err != nil {
		t.Fatal(err)
	}
	fetched, _ := s.Client.Do(ctx, http.MethodGet, "/v1/transfers/"+tr.ID, nil)
	if fetched.Status != 200 || !bytes.Equal(fetched.Body, posted.Body) {
		t.Fatalf("GET transfer differs from POST response:\n%s\n%s", fetched, posted)
	}
	if len(tr.Entries) != 2 || tr.Entries[0].Direction != "debit" || tr.Entries[0].Amount != -1 || tr.Entries[1].Amount != 1 {
		t.Fatalf("unexpected entries %+v", tr.Entries)
	}
}

func TestHealthAndRequestIDs(t *testing.T) {
	s := ledgertest.NewServer(t)
	ctx := context.Background()
	r, err := s.Client.Do(ctx, http.MethodGet, "/healthz", nil)
	if err != nil || r.Status != 200 || !strings.Contains(string(r.Body), `"status":"ok"`) {
		t.Fatalf("healthz: %v %s", err, r)
	}
	if id := r.Header.Get("X-Request-ID"); len(id) < 16 {
		t.Fatalf("generated request id %q", id)
	}
	r, _ = s.Client.Do(ctx, http.MethodGet, "/healthz", nil, "X-Request-ID", "client-chosen-id.42")
	if r.Header.Get("X-Request-ID") != "client-chosen-id.42" {
		t.Fatalf("client request id not echoed: %q", r.Header.Get("X-Request-ID"))
	}
	r, _ = s.Client.Do(ctx, http.MethodGet, "/healthz", nil, "X-Request-ID", "bad id with spaces")
	if r.Header.Get("X-Request-ID") == "bad id with spaces" {
		t.Fatal("malformed request id echoed back")
	}
}

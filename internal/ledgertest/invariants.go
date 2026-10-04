package ledgertest

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jsankalp715/Idempotent-Payments-Ledger-/internal/postgres"
)

// Summary describes the ledger state CheckInvariants inspected.
type Summary struct {
	Accounts       int
	Transfers      int
	Entries        int
	IdempotentKeys int
}

// CheckInvariants verifies, with independent SQL, the invariants the ledger
// promises. Every violation found is reported:
//
//	(a) money is conserved: per currency, all balances (system accounts
//	    included) sum to zero, so no money appeared or vanished
//	(b) no non-system account has a negative balance
//	(c) every account balance equals the sum of its entries and its latest
//	    balance_after, and every balance_after continues the previous one
//	(d) all entries sum to zero; every transfer has exactly two entries that
//	    sum to zero, debiting the source and crediting the destination
//	(e) every idempotency key created at most one transfer, every stored
//	    201 response names the transfer created with its key, and no stored
//	    rejection has a transfer behind it
func CheckInvariants(ctx context.Context, db postgres.DBTX) (Summary, error) {
	var errs []error
	violations := func(name, query string, args ...any) {
		rows, err := db.Query(ctx, query, args...)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: query failed: %w", name, err))
			return
		}
		defer rows.Close()
		var found []string
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				errs = append(errs, fmt.Errorf("%s: scan: %w", name, err))
				return
			}
			if len(found) < 10 {
				found = append(found, v)
			}
		}
		if err := rows.Err(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			return
		}
		if len(found) > 0 {
			errs = append(errs, fmt.Errorf("%s violated, e.g. %s", name, strings.Join(found, "; ")))
		}
	}

	violations("(a) money conserved per currency",
		`SELECT currency || ' sums to ' || sum(balance)
		   FROM accounts GROUP BY currency HAVING sum(balance) <> 0`)
	violations("(b) no negative balances",
		`SELECT id || ' has ' || balance FROM accounts WHERE NOT is_system AND balance < 0`)
	violations("(c) balance equals sum of entries",
		`SELECT a.id || ': balance ' || a.balance || ', entries sum ' || coalesce(s.total, 0) || ', last balance_after ' || coalesce(l.balance_after, 0)
		   FROM accounts a
		   LEFT JOIN (SELECT account_id, sum(amount) AS total FROM entries GROUP BY account_id) s ON s.account_id = a.id
		   LEFT JOIN LATERAL (SELECT balance_after FROM entries e WHERE e.account_id = a.id ORDER BY e.id DESC LIMIT 1) l ON true
		  WHERE a.balance <> coalesce(s.total, 0) OR a.balance <> coalesce(l.balance_after, 0)`)
	violations("(c) running balance chain",
		`SELECT 'entry ' || id || ': balance_after ' || balance_after || ' <> ' || prev || ' + ' || amount
		   FROM (SELECT id, amount, balance_after,
		                lag(balance_after, 1, 0::bigint) OVER (PARTITION BY account_id ORDER BY id) AS prev
		           FROM entries) x
		  WHERE balance_after <> prev + amount`)
	violations("(d) entries sum to zero",
		`SELECT 'all entries sum to ' || sum(amount) FROM entries HAVING coalesce(sum(amount), 0) <> 0`)
	violations("(d) two balanced entries per transfer",
		`SELECT t.id || ': ' || count(e.id) || ' entries, sum ' || coalesce(sum(e.amount), 0)
		   FROM transfers t LEFT JOIN entries e ON e.transfer_id = t.id
		  GROUP BY t.id, t.from_account_id, t.to_account_id, t.amount
		 HAVING count(e.id) <> 2
		     OR coalesce(sum(e.amount), 0) <> 0
		     OR NOT bool_or(e.account_id = t.from_account_id AND e.amount = -t.amount)
		     OR NOT bool_or(e.account_id = t.to_account_id AND e.amount = t.amount)`)
	violations("(d) no orphan entries",
		`SELECT 'entry ' || e.id FROM entries e LEFT JOIN transfers t ON t.id = e.transfer_id WHERE t.id IS NULL`)
	violations("(e) idempotency key applied at most once",
		`SELECT idempotency_key || ' created ' || count(*) || ' transfers'
		   FROM transfers WHERE idempotency_key IS NOT NULL
		  GROUP BY idempotency_key HAVING count(*) > 1`)
	violations("(e) stored 201 responses match their transfer",
		`SELECT k.key FROM idempotency_keys k
		  WHERE k.request_path = '/v1/transfers' AND k.response_status = 201
		    AND NOT EXISTS (SELECT 1 FROM transfers t
		                     WHERE t.idempotency_key = k.key
		                       AND t.id::text = convert_from(k.response_body, 'UTF8')::jsonb ->> 'id')`)
	violations("(e) stored rejections moved no money",
		`SELECT k.key || ' stored ' || k.response_status || ' but has a transfer'
		   FROM idempotency_keys k
		  WHERE k.request_path = '/v1/transfers' AND k.response_status <> 201
		    AND EXISTS (SELECT 1 FROM transfers t WHERE t.idempotency_key = k.key)`)

	var s Summary
	if err := db.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM accounts), (SELECT count(*) FROM transfers),
		        (SELECT count(*) FROM entries), (SELECT count(*) FROM idempotency_keys)`).
		Scan(&s.Accounts, &s.Transfers, &s.Entries, &s.IdempotentKeys); err != nil {
		errs = append(errs, fmt.Errorf("summary: %w", err))
	}
	return s, errors.Join(errs...)
}

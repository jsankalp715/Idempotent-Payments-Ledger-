package httpapi

import (
	"bytes"
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jsankalp715/Idempotent-Payments-Ledger-/internal/idempotency"
	"github.com/jsankalp715/Idempotent-Payments-Ledger-/internal/ledger"
)

type createTransferRequest struct {
	FromAccount *string `json:"from_account"`
	ToAccount   *string `json:"to_account"`
	Amount      *int64  `json:"amount"`
	Currency    *string `json:"currency"`
}

// transferFingerprint is the canonical, parsed form of a transfer request.
// Hashing it (rather than the raw body) makes formatting irrelevant: only a
// change of meaning counts as "a different payload".
type transferFingerprint struct {
	FromAccount string `json:"from_account"`
	ToAccount   string `json:"to_account"`
	Amount      int64  `json:"amount"`
	Currency    string `json:"currency"`
}

// toInput checks presence and syntax of every field and the domain rules,
// reporting every problem at once, in a stable field order.
func (req createTransferRequest) toInput() (ledger.TransferInput, error) {
	var in ledger.TransferInput
	overrides := map[string]string{} // field -> problem that hides domain problems
	parseAccount := func(field string, raw *string) uuid.UUID {
		if raw == nil {
			overrides[field] = "is required"
			return uuid.Nil
		}
		id, err := parseUUID(*raw)
		if err != nil {
			overrides[field] = "must be a UUID"
		}
		return id
	}
	in.FromAccountID = parseAccount("from_account", req.FromAccount)
	in.ToAccountID = parseAccount("to_account", req.ToAccount)
	if req.Amount == nil {
		overrides["amount"] = "is required"
	} else {
		in.Amount = *req.Amount
	}
	if req.Currency == nil {
		overrides["currency"] = "is required"
	} else {
		in.Currency = *req.Currency
	}
	return in, mergeProblems(in.Validate(), overrides, "from_account", "to_account", "amount", "currency")
}

// mergeProblems combines transport-level problems (missing or unparseable
// fields) with the domain validation result, field by field in order.
func mergeProblems(domainErr error, overrides map[string]string, order ...string) error {
	var domain ledger.ValidationError
	if d, ok := domainErr.(*ledger.ValidationError); ok {
		domain = *d
	}
	var out ledger.ValidationError
	for _, field := range order {
		if msg, ok := overrides[field]; ok {
			out.Add(field, msg)
			continue
		}
		for _, p := range domain.Fields {
			if p.Field == field {
				out.Add(p.Field, p.Message)
			}
		}
	}
	return out.Err()
}

func (s *server) createTransfer(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Idempotency-Key")
	if err := idempotency.ValidateKey(key); err != nil {
		s.writeErr(w, r, err)
		return
	}
	var req createTransferRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.writeErr(w, r, err)
		return
	}
	in, err := req.toInput()
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	hash, err := idempotency.Fingerprint(http.MethodPost, "/v1/transfers", transferFingerprint{
		FromAccount: in.FromAccountID.String(),
		ToAccount:   in.ToAccountID.String(),
		Amount:      in.Amount,
		Currency:    in.Currency,
	})
	if err != nil {
		s.writeErr(w, r, err)
		return
	}

	var rebuilt bool
	resp, replayed, err := s.idem.Do(r.Context(), idempotency.Request{
		Key: key, Method: http.MethodPost, Path: "/v1/transfers", Hash: hash,
	}, func(ctx context.Context, tx pgx.Tx) (idempotency.Response, error) {
		rebuilt = false
		// The idempotency record may have expired, but the transfer row keeps
		// its key forever: a key never moves money twice. For the same request
		// the original response is rebuilt from the immutable rows.
		existing, storedHash, found, err := s.ledger.TransferByKey(ctx, tx, key)
		if err != nil {
			return idempotency.Response{}, err
		}
		if found {
			if !bytes.Equal(storedHash, hash) {
				return idempotency.Response{}, idempotency.ErrKeyReused
			}
			rebuilt = true
			return idempotency.Response{Status: http.StatusCreated, Body: renderJSON(toTransferJSON(existing))}, nil
		}

		t, err := s.ledger.Transfer(ctx, tx, in, key, hash)
		var domain *ledger.Error
		if errors.As(err, &domain) {
			// A definitive business outcome: stored and replayed like a success,
			// so a retry can never turn a rejected transfer into an executed one.
			return idempotency.Response{
				Status: http.StatusUnprocessableEntity,
				Body:   renderError(domain.Code, domain.Message, nil),
			}, nil
		}
		if err != nil {
			return idempotency.Response{}, err
		}
		return idempotency.Response{Status: http.StatusCreated, Body: renderJSON(toTransferJSON(t))}, nil
	})
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	if replayed || rebuilt {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	writeBody(w, resp.Status, resp.Body)
}

func (s *server) getTransfer(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r)
	if !ok {
		return
	}
	t, err := s.ledger.GetTransfer(r.Context(), s.runner.Pool(), id)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toTransferJSON(t))
}

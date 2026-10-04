package httpapi

import (
	"context"
	"net/http"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jsankalp715/Idempotent-Payments-Ledger-/internal/idempotency"
	"github.com/jsankalp715/Idempotent-Payments-Ledger-/internal/ledger"
	"github.com/jsankalp715/Idempotent-Payments-Ledger-/internal/postgres"
)

const (
	defaultPageSize = 50
	maxPageSize     = 200
)

type createAccountRequest struct {
	Currency       *string `json:"currency"`
	InitialBalance *int64  `json:"initial_balance"`
}

// accountFingerprint is the canonical form hashed for idempotent creation.
type accountFingerprint struct {
	Currency       string `json:"currency"`
	InitialBalance int64  `json:"initial_balance"`
}

func (s *server) createAccount(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Idempotency-Key")
	if key != "" {
		if err := idempotency.ValidateKey(key); err != nil {
			s.writeErr(w, r, err)
			return
		}
	}
	var req createAccountRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.writeErr(w, r, err)
		return
	}
	in := ledger.CreateAccountInput{}
	overrides := map[string]string{}
	if req.Currency == nil {
		overrides["currency"] = "is required"
	} else {
		in.Currency = *req.Currency
	}
	if req.InitialBalance != nil {
		in.InitialBalance = *req.InitialBalance
	}
	if err := mergeProblems(in.Validate(), overrides, "currency", "initial_balance"); err != nil {
		s.writeErr(w, r, err)
		return
	}

	work := func(ctx context.Context, tx pgx.Tx) (idempotency.Response, error) {
		acc, err := s.ledger.CreateAccount(ctx, tx, in)
		if err != nil {
			return idempotency.Response{}, err
		}
		return idempotency.Response{Status: http.StatusCreated, Body: renderJSON(toAccountJSON(acc))}, nil
	}

	// Account creation works without a key, but a client that may retry
	// (initial funding moves money) should send one.
	if key == "" {
		resp, err := postgres.InTx(r.Context(), s.runner, work)
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
		writeBody(w, resp.Status, resp.Body)
		return
	}
	hash, err := idempotency.Fingerprint(http.MethodPost, "/v1/accounts", accountFingerprint{in.Currency, in.InitialBalance})
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	resp, replayed, err := s.idem.Do(r.Context(), idempotency.Request{
		Key: key, Method: http.MethodPost, Path: "/v1/accounts", Hash: hash,
	}, work)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	if replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	writeBody(w, resp.Status, resp.Body)
}

func (s *server) getAccount(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r)
	if !ok {
		return
	}
	acc, err := s.ledger.GetAccount(r.Context(), s.runner.Pool(), id)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toAccountJSON(acc))
}

func (s *server) listEntries(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r)
	if !ok {
		return
	}
	limit := defaultPageSize
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxPageSize {
			s.writeErr(w, r, badRequest("invalid_query", "limit must be an integer between 1 and %d", maxPageSize))
			return
		}
		limit = n
	}
	var before int64
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 1 {
			s.writeErr(w, r, badRequest("invalid_query", "cursor must be a value returned as next_cursor"))
			return
		}
		before = n
	}
	// Fetch one extra row to learn whether another page exists.
	entries, err := s.ledger.ListEntries(r.Context(), s.runner.Pool(), id, before, limit+1)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	out := entryListJSON{Entries: make([]entryJSON, 0, limit)}
	if len(entries) > limit {
		entries = entries[:limit]
		next := strconv.FormatInt(entries[limit-1].ID, 10)
		out.NextCursor = &next
	}
	for _, e := range entries {
		out.Entries = append(out.Entries, toEntryJSON(e))
	}
	writeJSON(w, http.StatusOK, out)
}

// pathUUID parses the {id} path segment, answering 400 itself if it is not a
// canonical UUID.
func (s *server) pathUUID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := parseUUID(r.PathValue("id"))
	if err != nil {
		s.writeErr(w, r, badRequest("invalid_id", "id must be a UUID"))
		return uuid.Nil, false
	}
	return id, true
}

// parseUUID accepts only the canonical 36-character form, in either case.
func parseUUID(s string) (uuid.UUID, error) {
	if len(s) != 36 {
		return uuid.Nil, strconv.ErrSyntax
	}
	return uuid.Parse(s)
}

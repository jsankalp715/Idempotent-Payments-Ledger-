// Package idempotency makes request execution exactly-once per idempotency key.
//
// Store.Do runs the request's work and records the key, the request hash and
// the exact response in one database transaction:
//
//  1. pg_try_advisory_xact_lock(hash(key)): if another transaction holds it,
//     a request with this key is in flight and Do fails with ErrInFlight
//     (HTTP 409) immediately instead of queueing behind it.
//  2. Look the key up. A live record with the same request hash is replayed
//     (same status, same body bytes) without executing anything; a different
//     hash fails with ErrKeyReused (HTTP 422). Expired records are discarded.
//  3. Otherwise run the work and insert the key record in the same
//     transaction. The record exists if and only if the work committed: a
//     crash, error or timeout before COMMIT leaves neither, and the client's
//     retry executes from scratch.
//
// Only definitive responses are stored. Errors returned by the work (as
// opposed to responses it renders) roll everything back and are not cached,
// so transient failures stay retryable with the same key.
package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jsankalp715/Idempotent-Payments-Ledger-/internal/postgres"
)

// KeyConstraint is the primary key of idempotency_keys. A violation means a
// concurrent transaction stored the same key first, so the transaction is
// retried and the retry replays the stored response.
const KeyConstraint = "idempotency_keys_pkey"

// MaxKeyLength is the longest accepted idempotency key.
const MaxKeyLength = 255

var (
	// ErrInFlight means another request with the same key is executing now.
	ErrInFlight = errors.New("a request with this idempotency key is already in flight")
	// ErrKeyReused means the key was already used with a different request.
	ErrKeyReused = errors.New("idempotency key was already used with a different request")
	// ErrInvalidKey means the key is missing or malformed.
	ErrInvalidKey = errors.New("invalid idempotency key")
)

// ValidateKey accepts 1 to MaxKeyLength printable ASCII characters without
// spaces, which covers UUIDs, ULIDs and other common key formats.
func ValidateKey(key string) error {
	if key == "" {
		return fmt.Errorf("%w: the Idempotency-Key header is required", ErrInvalidKey)
	}
	if len(key) > MaxKeyLength {
		return fmt.Errorf("%w: longer than %d characters", ErrInvalidKey, MaxKeyLength)
	}
	for i := 0; i < len(key); i++ {
		if c := key[i]; c < 0x21 || c > 0x7e {
			return fmt.Errorf("%w: only printable ASCII characters without spaces are allowed", ErrInvalidKey)
		}
	}
	return nil
}

// Fingerprint returns the SHA-256 request hash stored with a key. It covers
// the method, the path and a canonical JSON encoding of the parsed request,
// so the same request sent with different whitespace or field order hashes
// identically, while any change of a value, the endpoint or the method does
// not. canonical must encode deterministically (structs, not maps).
func Fingerprint(method, path string, canonical any) ([]byte, error) {
	body, err := json.Marshal(canonical)
	if err != nil {
		return nil, fmt.Errorf("encode canonical request: %w", err)
	}
	h := sha256.New()
	// Length-prefix each part so no two different inputs share an encoding.
	for _, part := range [][]byte{[]byte("ledger-idempotency-v1"), []byte(method), []byte(path), body} {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(part)))
		h.Write(n[:])
		h.Write(part)
	}
	return h.Sum(nil), nil
}

// LockID maps a key to the 64-bit advisory lock id that serializes requests
// with that key. Distinct keys colliding is a 2^-64 event whose only effect is
// a spurious, retryable 409.
func LockID(key string) int64 {
	sum := sha256.Sum256([]byte("ledger-idempotency-lock:" + key))
	return int64(binary.BigEndian.Uint64(sum[:8]))
}

// Request identifies an idempotent request.
type Request struct {
	Key    string
	Method string
	Path   string
	Hash   []byte
}

// Response is a stored response, replayed byte for byte.
type Response struct {
	Status int
	Body   []byte
}

// Store executes and records idempotent requests.
type Store struct {
	runner *postgres.TxRunner
	ttl    time.Duration
	logger *slog.Logger
}

// NewStore returns a Store whose records live for ttl.
func NewStore(runner *postgres.TxRunner, ttl time.Duration, logger *slog.Logger) *Store {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Store{runner: runner, ttl: ttl, logger: logger}
}

// Work executes a request inside the transaction that will also store its
// key record. It returns the response to send and store, or an error to roll
// everything back without storing anything.
type Work func(ctx context.Context, tx pgx.Tx) (Response, error)

// Do executes work at most once per key; see the package documentation.
// replayed reports whether resp is a stored response rather than the result
// of running work now.
func (s *Store) Do(ctx context.Context, req Request, work Work) (resp Response, replayed bool, err error) {
	if err := ValidateKey(req.Key); err != nil {
		return Response{}, false, err
	}
	if len(req.Hash) != sha256.Size {
		return Response{}, false, fmt.Errorf("request hash must be %d bytes, got %d", sha256.Size, len(req.Hash))
	}
	err = s.runner.Run(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// Reset per attempt: the runner may run this function several times.
		resp, replayed = Response{}, false

		var locked bool
		if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, LockID(req.Key)).Scan(&locked); err != nil {
			return fmt.Errorf("acquire idempotency lock: %w", err)
		}
		if !locked {
			return ErrInFlight
		}

		var (
			storedHash []byte
			stored     Response
			expired    bool
		)
		err := tx.QueryRow(ctx,
			`SELECT request_hash, response_status, response_body, expires_at <= now()
			   FROM idempotency_keys WHERE key = $1`, req.Key).
			Scan(&storedHash, &stored.Status, &stored.Body, &expired)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return fmt.Errorf("look up idempotency key: %w", err)
		case !expired:
			if !bytes.Equal(storedHash, req.Hash) {
				return ErrKeyReused
			}
			resp, replayed = stored, true
			return nil
		default:
			// Expired but not yet cleaned up: behave as if it were gone.
			if _, err := tx.Exec(ctx, `DELETE FROM idempotency_keys WHERE key = $1`, req.Key); err != nil {
				return fmt.Errorf("delete expired idempotency key: %w", err)
			}
		}

		out, err := work(ctx, tx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO idempotency_keys (key, request_method, request_path, request_hash, response_status, response_body, expires_at)
			 VALUES ($1, $2, $3, $4, $5, $6, now() + make_interval(secs => $7))`,
			req.Key, req.Method, req.Path, req.Hash, out.Status, out.Body, s.ttl.Seconds()); err != nil {
			return fmt.Errorf("store idempotency key: %w", err)
		}
		resp = out
		return nil
	})
	if err != nil {
		return Response{}, false, err
	}
	return resp, replayed, nil
}

// DeleteExpired removes expired records in batches of batchSize until none
// are left, returning how many were deleted. SKIP LOCKED lets several
// instances clean up concurrently without blocking each other or requests.
func (s *Store) DeleteExpired(ctx context.Context, batchSize int) (int64, error) {
	var total int64
	for {
		tag, err := s.runner.Pool().Exec(ctx,
			`DELETE FROM idempotency_keys
			  WHERE key IN (SELECT key FROM idempotency_keys
			                 WHERE expires_at <= now()
			                 ORDER BY expires_at
			                 LIMIT $1
			                 FOR UPDATE SKIP LOCKED)`, batchSize)
		if err != nil {
			return total, fmt.Errorf("delete expired idempotency keys: %w", err)
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < int64(batchSize) {
			return total, nil
		}
		if err := ctx.Err(); err != nil {
			return total, err
		}
	}
}

// RunCleanup calls DeleteExpired immediately and then every interval until
// ctx is done.
func (s *Store) RunCleanup(ctx context.Context, interval time.Duration, batchSize int) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		n, err := s.DeleteExpired(ctx, batchSize)
		switch {
		case err != nil && ctx.Err() == nil:
			s.logger.ErrorContext(ctx, "idempotency key cleanup failed", "error", err, "deleted", n)
		case n > 0:
			s.logger.InfoContext(ctx, "deleted expired idempotency keys", "deleted", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Package ledgertest is shared support for the black-box integration and
// stress tests: an in-process server over a private test schema, a small API
// client and a checker for the ledger's global invariants.
package ledgertest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jsankalp715/Idempotent-Payments-Ledger-/internal/app"
	"github.com/jsankalp715/Idempotent-Payments-Ledger-/internal/config"
	"github.com/jsankalp715/Idempotent-Payments-Ledger-/internal/testdb"
)

// Server is the ledger service running in-process on a real TCP listener
// over a private, migrated schema.
type Server struct {
	DB     *testdb.DB
	App    *app.App
	HTTP   *httptest.Server
	Config config.Config
	Client *Client
}

// Options tune NewServer.
type Options struct {
	// MaxConns is the database pool size (default 20).
	MaxConns int32
	// Configure may adjust the service configuration.
	Configure func(*config.Config)
}

// TestConfig is the service configuration used by tests: the normal defaults,
// overridable through the usual environment variables (so CI can run the
// whole suite with TX_ISOLATION=serializable), and faster retries.
func TestConfig(tb testing.TB) config.Config {
	tb.Helper()
	cfg, err := config.Load(func(k string) string {
		if k == "DATABASE_URL" {
			return "provided-by-testdb"
		}
		if v := os.Getenv(k); v != "" {
			return v
		}
		switch k {
		case "TX_RETRY_BASE_DELAY":
			return "2ms"
		case "TX_RETRY_MAX_DELAY":
			return "50ms"
		case "REQUEST_TIMEOUT":
			return "30s"
		}
		return ""
	})
	if err != nil {
		tb.Fatalf("test config: %v", err)
	}
	return cfg
}

// NewServer starts the service against a fresh test schema.
func NewServer(tb testing.TB, opts ...Options) *Server {
	tb.Helper()
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	if o.MaxConns == 0 {
		o.MaxConns = 20
	}
	db := testdb.New(tb, testdb.Options{MaxConns: o.MaxConns})
	cfg := TestConfig(tb)
	if o.Configure != nil {
		o.Configure(&cfg)
	}
	logger := slog.New(slog.DiscardHandler)
	if os.Getenv("LEDGER_TEST_LOG") == "1" {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	a := app.NewWithPool(cfg, db.Pool, logger)
	srv := httptest.NewServer(a.Handler())
	tb.Cleanup(srv.Close)
	return &Server{DB: db, App: a, HTTP: srv, Config: cfg, Client: NewClient(srv.URL, srv.Client())}
}

// Client is a minimal client for the ledger API.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// NewClient returns a client for the API at baseURL.
func NewClient(baseURL string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{BaseURL: baseURL, HTTP: hc}
}

// Response is a fully read HTTP response.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// ErrorCode returns error.code from an error envelope, or "".
func (r Response) ErrorCode() string {
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(r.Body, &env)
	return env.Error.Code
}

// Decode unmarshals the body into v.
func (r Response) Decode(v any) error {
	if err := json.Unmarshal(r.Body, v); err != nil {
		return fmt.Errorf("decode %d response %q: %w", r.Status, r.Body, err)
	}
	return nil
}

func (r Response) String() string {
	return fmt.Sprintf("%d %s", r.Status, bytes.TrimSpace(r.Body))
}

// Do sends a request. body may be nil, a []byte (sent verbatim) or a value
// encoded as JSON. Header pairs are given as "Name", "value", ...
func (c *Client) Do(ctx context.Context, method, path string, body any, header ...string) (Response, error) {
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	default:
		enc, err := json.Marshal(b)
		if err != nil {
			return Response{}, err
		}
		rd = bytes.NewReader(enc)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rd)
	if err != nil {
		return Response{}, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return Response{}, err
	}
	return Response{Status: resp.StatusCode, Header: resp.Header, Body: data}, nil
}

// Account is the API representation of an account.
type Account struct {
	ID        string `json:"id"`
	Currency  string `json:"currency"`
	Type      string `json:"type"`
	Balance   int64  `json:"balance"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// Transfer is the API representation of a transfer.
type Transfer struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	FromAccount string `json:"from_account"`
	ToAccount   string `json:"to_account"`
	Amount      int64  `json:"amount"`
	Currency    string `json:"currency"`
	CreatedAt   string `json:"created_at"`
	Entries     []struct {
		ID        int64  `json:"id"`
		AccountID string `json:"account_id"`
		Direction string `json:"direction"`
		Amount    int64  `json:"amount"`
	} `json:"entries"`
}

// TransferRequest is the body of POST /v1/transfers.
type TransferRequest struct {
	FromAccount string `json:"from_account"`
	ToAccount   string `json:"to_account"`
	Amount      int64  `json:"amount"`
	Currency    string `json:"currency"`
}

// CreateAccount creates an account and fails tb on any error.
func (c *Client) CreateAccount(tb testing.TB, currency string, initialBalance int64) Account {
	tb.Helper()
	resp, err := c.Do(context.Background(), http.MethodPost, "/v1/accounts",
		map[string]any{"currency": currency, "initial_balance": initialBalance})
	if err != nil {
		tb.Fatalf("create account: %v", err)
	}
	if resp.Status != http.StatusCreated {
		tb.Fatalf("create account: %s", resp)
	}
	var a Account
	if err := resp.Decode(&a); err != nil {
		tb.Fatal(err)
	}
	return a
}

// Transfer posts a transfer with the given idempotency key.
func (c *Client) Transfer(ctx context.Context, key string, req TransferRequest) (Response, error) {
	return c.Do(ctx, http.MethodPost, "/v1/transfers", req, "Idempotency-Key", key)
}

// MustTransfer posts a transfer and fails tb on a transport error.
func (c *Client) MustTransfer(tb testing.TB, key string, req TransferRequest) Response {
	tb.Helper()
	resp, err := c.Transfer(context.Background(), key, req)
	if err != nil {
		tb.Fatalf("transfer: %v", err)
	}
	return resp
}

// Balance returns an account's balance and fails tb on any error.
func (c *Client) Balance(tb testing.TB, id string) int64 {
	tb.Helper()
	resp, err := c.Do(context.Background(), http.MethodGet, "/v1/accounts/"+id, nil)
	if err != nil {
		tb.Fatalf("get account: %v", err)
	}
	if resp.Status != http.StatusOK {
		tb.Fatalf("get account: %s", resp)
	}
	var a Account
	if err := resp.Decode(&a); err != nil {
		tb.Fatal(err)
	}
	return a.Balance
}

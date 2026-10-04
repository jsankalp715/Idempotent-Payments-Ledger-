// Package testdb gives each test a private, fully migrated PostgreSQL schema.
//
// Tests that need a database call New. The database comes from DATABASE_URL.
// When it is unset the test is skipped, unless LEDGER_REQUIRE_DB=1 (as in CI),
// in which case the test fails: CI can never pass by silently skipping the
// database tests.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jsankalp715/Idempotent-Payments-Ledger-/internal/postgres"
)

// Options tunes the pool returned by New.
type Options struct {
	MaxConns int32
}

// DB is a migrated schema plus a pool whose connections use it.
type DB struct {
	Pool   *pgxpool.Pool
	Schema string
	URL    string
}

// URL returns DATABASE_URL, skipping or failing tb when it is not set.
func URL(tb testing.TB) string {
	tb.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		if os.Getenv("LEDGER_REQUIRE_DB") == "1" {
			tb.Fatal("DATABASE_URL is not set but LEDGER_REQUIRE_DB=1")
		}
		tb.Skip("DATABASE_URL not set; skipping test that needs PostgreSQL")
	}
	return url
}

// New creates a uniquely named schema, applies all migrations to it and
// returns a pool bound to it. The schema is dropped when the test ends
// (set LEDGER_KEEP_TEST_SCHEMA=1 to keep it for debugging).
func New(tb testing.TB, opts ...Options) *DB {
	tb.Helper()
	url := URL(tb)
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	if o.MaxConns == 0 {
		o.MaxConns = 10
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	schema := "test_" + randomHex(8)
	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		tb.Fatalf("connect to %s: %v", redact(url), err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		_ = admin.Close(ctx)
		tb.Fatalf("create schema: %v", err)
	}
	_ = admin.Close(ctx)

	pool, err := postgres.NewPool(ctx, url, postgres.PoolOptions{
		MaxConns:        o.MaxConns,
		SearchPath:      schema,
		ApplicationName: "ledger-test",
	})
	if err != nil {
		dropSchema(tb, url, schema)
		tb.Fatalf("open pool: %v", err)
	}
	tb.Cleanup(func() {
		pool.Close()
		if os.Getenv("LEDGER_KEEP_TEST_SCHEMA") == "1" {
			tb.Logf("keeping test schema %s", schema)
			return
		}
		dropSchema(tb, url, schema)
	})

	if err := postgres.Migrate(ctx, pool, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		tb.Fatalf("migrate schema %s: %v", schema, err)
	}
	return &DB{Pool: pool, Schema: schema, URL: url}
}

// Conn opens a dedicated connection to the test schema, outside the pool.
// Tests use it to hold locks or run raw SQL while the code under test works.
func (db *DB) Conn(tb testing.TB) *pgx.Conn {
	tb.Helper()
	cfg, err := pgx.ParseConfig(db.URL)
	if err != nil {
		tb.Fatalf("parse url: %v", err)
	}
	cfg.RuntimeParams["search_path"] = db.Schema
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		tb.Fatalf("connect: %v", err)
	}
	tb.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

func dropSchema(tb testing.TB, url, schema string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		tb.Errorf("drop schema %s: connect: %v", schema, err)
		return
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
		tb.Errorf("drop schema %s: %v", schema, err)
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	return hex.EncodeToString(b)
}

func redact(url string) string {
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		return "<unparseable DATABASE_URL>"
	}
	return fmt.Sprintf("postgres://%s@%s:%d/%s", cfg.User, cfg.Host, cfg.Port, cfg.Database)
}

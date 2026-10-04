// Package postgres owns the connection pool, the embedded schema migrations
// and the retrying transaction runner used by every write path.
package postgres

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/binary"
	"fmt"
	"io/fs"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgconn/ctxwatch"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

// DBTX is the query surface shared by *pgxpool.Pool, *pgxpool.Conn and
// pgx.Tx, so read helpers work both inside and outside a transaction.
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// PoolOptions configures NewPool.
type PoolOptions struct {
	MaxConns int32
	MinConns int32
	// SearchPath, if set, becomes the search_path of every connection. The
	// test suite uses it to give each test a private schema.
	SearchPath string
	// ApplicationName shows up in pg_stat_activity.
	ApplicationName string
}

// NewPool connects to databaseURL and verifies the connection with a ping.
func NewPool(ctx context.Context, databaseURL string, opts PoolOptions) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	if opts.MaxConns > 0 {
		cfg.MaxConns = opts.MaxConns
	}
	if opts.MinConns > 0 {
		cfg.MinConns = opts.MinConns
	}
	if opts.ApplicationName == "" {
		opts.ApplicationName = "ledger"
	}
	cfg.ConnConfig.RuntimeParams["application_name"] = opts.ApplicationName
	if opts.SearchPath != "" {
		cfg.ConnConfig.RuntimeParams["search_path"] = opts.SearchPath
	}
	// When a request's context is cancelled (client gone, timeout, shutdown),
	// ask the server to cancel the running statement instead of just closing
	// the socket. A backend blocked on a lock would otherwise keep waiting,
	// and keep the locks it already holds (including the idempotency key
	// lock, which makes the client's retry see 409), until it next tried to
	// talk to the dead socket. The cancelled transaction rolls back and the
	// connection stays usable.
	cfg.ConnConfig.BuildContextWatcherHandler = func(c *pgconn.PgConn) ctxwatch.Handler {
		return &pgconn.CancelRequestContextWatcherHandler{Conn: c, DeadlineDelay: time.Second}
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Migrate applies all pending migrations to the pool's current schema. A
// session-level advisory lock, derived from the schema name, keeps several
// instances that start at the same time from migrating concurrently.
func Migrate(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) error {
	var schema string
	if err := pool.QueryRow(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
		return fmt.Errorf("read current schema: %w", err)
	}
	sum := sha256.Sum256([]byte("ledger-migrations:" + schema))
	locker, err := lock.NewPostgresSessionLocker(
		lock.WithLockID(int64(binary.BigEndian.Uint64(sum[:8]))),
		lock.WithLockTimeout(1, 120),
	)
	if err != nil {
		return fmt.Errorf("create migration locker: %w", err)
	}

	fsys, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		return fmt.Errorf("open embedded migrations: %w", err)
	}
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()

	provider, err := goose.NewProvider(goose.DialectPostgres, db, fsys, goose.WithSessionLocker(locker))
	if err != nil {
		return fmt.Errorf("create migration provider: %w", err)
	}
	results, err := provider.Up(ctx)
	if err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	for _, r := range results {
		logger.Info("applied migration", "version", r.Source.Version, "file", r.Source.Path, "duration", r.Duration)
	}
	return nil
}

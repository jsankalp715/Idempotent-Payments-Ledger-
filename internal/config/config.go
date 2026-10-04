// Package config loads the service configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Config is the complete runtime configuration of the ledger service.
type Config struct {
	// DatabaseURL is a libpq-style connection string. Required.
	DatabaseURL string
	// HTTPAddr is the listen address of the HTTP server.
	HTTPAddr string

	LogLevel  slog.Level
	LogFormat string // "json" or "text"

	DBMaxConns int32
	DBMinConns int32

	// TxIsolation is the isolation level of write transactions. Read committed
	// is the default: correctness comes from explicit row locks, not from the
	// isolation level. Serializable is supported and exercised by the stress
	// test to prove the retry path.
	TxIsolation      pgx.TxIsoLevel
	TxMaxAttempts    int
	TxRetryBaseDelay time.Duration
	TxRetryMaxDelay  time.Duration

	IdempotencyTTL          time.Duration
	IdempotencyCleanupEvery time.Duration
	IdempotencyCleanupBatch int

	RequestTimeout     time.Duration
	ShutdownTimeout    time.Duration
	ShutdownDrainDelay time.Duration

	MigrateOnStart bool
}

// Load reads the configuration through getenv (normally os.Getenv). Every
// invalid variable is reported, not just the first one.
func Load(getenv func(string) string) (Config, error) {
	p := parser{getenv: getenv}
	cfg := Config{
		DatabaseURL:             p.str("DATABASE_URL", ""),
		HTTPAddr:                p.str("HTTP_ADDR", ":8080"),
		LogLevel:                p.level("LOG_LEVEL", slog.LevelInfo),
		LogFormat:               p.oneOf("LOG_FORMAT", "json", "json", "text"),
		DBMaxConns:              int32(p.intRange("DB_MAX_CONNS", 20, 1, 1000)),
		DBMinConns:              int32(p.intRange("DB_MIN_CONNS", 2, 0, 1000)),
		TxIsolation:             p.isolation("TX_ISOLATION", pgx.ReadCommitted),
		TxMaxAttempts:           p.intRange("TX_MAX_ATTEMPTS", 8, 1, 100),
		TxRetryBaseDelay:        p.duration("TX_RETRY_BASE_DELAY", 5*time.Millisecond),
		TxRetryMaxDelay:         p.duration("TX_RETRY_MAX_DELAY", 250*time.Millisecond),
		IdempotencyTTL:          p.duration("IDEMPOTENCY_TTL", 24*time.Hour),
		IdempotencyCleanupEvery: p.duration("IDEMPOTENCY_CLEANUP_INTERVAL", 10*time.Minute),
		IdempotencyCleanupBatch: p.intRange("IDEMPOTENCY_CLEANUP_BATCH", 1000, 1, 100000),
		RequestTimeout:          p.duration("REQUEST_TIMEOUT", 10*time.Second),
		ShutdownTimeout:         p.duration("SHUTDOWN_TIMEOUT", 20*time.Second),
		ShutdownDrainDelay:      p.durationAllowZero("SHUTDOWN_DRAIN_DELAY", 0),
		MigrateOnStart:          p.boolean("MIGRATE_ON_START", true),
	}

	if cfg.DatabaseURL == "" {
		p.errs = append(p.errs, errors.New("DATABASE_URL is required"))
	}
	if cfg.DBMinConns > cfg.DBMaxConns {
		p.errs = append(p.errs, fmt.Errorf("DB_MIN_CONNS (%d) must not exceed DB_MAX_CONNS (%d)", cfg.DBMinConns, cfg.DBMaxConns))
	}
	if cfg.IdempotencyTTL < time.Second {
		p.errs = append(p.errs, fmt.Errorf("IDEMPOTENCY_TTL (%s) must be at least 1s", cfg.IdempotencyTTL))
	}
	if cfg.TxRetryBaseDelay > cfg.TxRetryMaxDelay {
		p.errs = append(p.errs, fmt.Errorf("TX_RETRY_BASE_DELAY (%s) must not exceed TX_RETRY_MAX_DELAY (%s)", cfg.TxRetryBaseDelay, cfg.TxRetryMaxDelay))
	}
	if err := errors.Join(p.errs...); err != nil {
		return Config{}, fmt.Errorf("invalid configuration: %w", err)
	}
	return cfg, nil
}

type parser struct {
	getenv func(string) string
	errs   []error
}

func (p *parser) lookup(key string) (string, bool) {
	v := strings.TrimSpace(p.getenv(key))
	return v, v != ""
}

func (p *parser) fail(key, value, want string) {
	p.errs = append(p.errs, fmt.Errorf("%s=%q: want %s", key, value, want))
}

func (p *parser) str(key, def string) string {
	if v, ok := p.lookup(key); ok {
		return v
	}
	return def
}

func (p *parser) oneOf(key, def string, allowed ...string) string {
	v, ok := p.lookup(key)
	if !ok {
		return def
	}
	v = strings.ToLower(v)
	for _, a := range allowed {
		if v == a {
			return v
		}
	}
	p.fail(key, v, "one of "+strings.Join(allowed, ", "))
	return def
}

func (p *parser) intRange(key string, def, minVal, maxVal int) int {
	v, ok := p.lookup(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < minVal || n > maxVal {
		p.fail(key, v, fmt.Sprintf("an integer in [%d, %d]", minVal, maxVal))
		return def
	}
	return n
}

func (p *parser) duration(key string, def time.Duration) time.Duration {
	d := p.durationAllowZero(key, def)
	if d == 0 {
		p.fail(key, p.getenv(key), "a positive duration")
		return def
	}
	return d
}

func (p *parser) durationAllowZero(key string, def time.Duration) time.Duration {
	v, ok := p.lookup(key)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		p.fail(key, v, `a non-negative Go duration such as "250ms" or "24h"`)
		return def
	}
	return d
}

func (p *parser) boolean(key string, def bool) bool {
	v, ok := p.lookup(key)
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		p.fail(key, v, "true or false")
		return def
	}
	return b
}

func (p *parser) level(key string, def slog.Level) slog.Level {
	v, ok := p.lookup(key)
	if !ok {
		return def
	}
	var l slog.Level
	if err := l.UnmarshalText([]byte(v)); err != nil {
		p.fail(key, v, "debug, info, warn or error")
		return def
	}
	return l
}

func (p *parser) isolation(key string, def pgx.TxIsoLevel) pgx.TxIsoLevel {
	v, ok := p.lookup(key)
	if !ok {
		return def
	}
	switch strings.NewReplacer("-", "_", " ", "_").Replace(strings.ToLower(v)) {
	case "read_committed":
		return pgx.ReadCommitted
	case "repeatable_read":
		return pgx.RepeatableRead
	case "serializable":
		return pgx.Serializable
	}
	p.fail(key, v, "read_committed, repeatable_read or serializable")
	return def
}

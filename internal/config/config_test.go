package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func env(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(env(map[string]string{"DATABASE_URL": "postgres://x"}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"HTTPAddr", cfg.HTTPAddr, ":8080"},
		{"LogLevel", cfg.LogLevel, slog.LevelInfo},
		{"LogFormat", cfg.LogFormat, "json"},
		{"DBMaxConns", cfg.DBMaxConns, int32(20)},
		{"DBMinConns", cfg.DBMinConns, int32(2)},
		{"TxIsolation", cfg.TxIsolation, pgx.ReadCommitted},
		{"TxMaxAttempts", cfg.TxMaxAttempts, 8},
		{"TxRetryBaseDelay", cfg.TxRetryBaseDelay, 5 * time.Millisecond},
		{"TxRetryMaxDelay", cfg.TxRetryMaxDelay, 250 * time.Millisecond},
		{"IdempotencyTTL", cfg.IdempotencyTTL, 24 * time.Hour},
		{"IdempotencyCleanupEvery", cfg.IdempotencyCleanupEvery, 10 * time.Minute},
		{"IdempotencyCleanupBatch", cfg.IdempotencyCleanupBatch, 1000},
		{"RequestTimeout", cfg.RequestTimeout, 10 * time.Second},
		{"ShutdownTimeout", cfg.ShutdownTimeout, 20 * time.Second},
		{"ShutdownDrainDelay", cfg.ShutdownDrainDelay, time.Duration(0)},
		{"MigrateOnStart", cfg.MigrateOnStart, true},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestLoadOverrides(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"DATABASE_URL":         "postgres://y",
		"HTTP_ADDR":            "127.0.0.1:9000",
		"LOG_LEVEL":            "debug",
		"LOG_FORMAT":           "TEXT",
		"DB_MAX_CONNS":         "50",
		"DB_MIN_CONNS":         "0",
		"TX_ISOLATION":         "Serializable",
		"TX_MAX_ATTEMPTS":      "3",
		"TX_RETRY_BASE_DELAY":  "1ms",
		"TX_RETRY_MAX_DELAY":   "1s",
		"IDEMPOTENCY_TTL":      "1h",
		"SHUTDOWN_DRAIN_DELAY": "2s",
		"MIGRATE_ON_START":     "false",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTPAddr != "127.0.0.1:9000" || cfg.LogLevel != slog.LevelDebug || cfg.LogFormat != "text" {
		t.Errorf("unexpected server/log config: %+v", cfg)
	}
	if cfg.DBMaxConns != 50 || cfg.DBMinConns != 0 {
		t.Errorf("unexpected pool config: max=%d min=%d", cfg.DBMaxConns, cfg.DBMinConns)
	}
	if cfg.TxIsolation != pgx.Serializable || cfg.TxMaxAttempts != 3 {
		t.Errorf("unexpected tx config: %v %d", cfg.TxIsolation, cfg.TxMaxAttempts)
	}
	if cfg.IdempotencyTTL != time.Hour || cfg.ShutdownDrainDelay != 2*time.Second || cfg.MigrateOnStart {
		t.Errorf("unexpected misc config: %+v", cfg)
	}
}

func TestLoadIsolationAliases(t *testing.T) {
	for in, want := range map[string]pgx.TxIsoLevel{
		"read_committed":  pgx.ReadCommitted,
		"read committed":  pgx.ReadCommitted,
		"READ-COMMITTED":  pgx.ReadCommitted,
		"repeatable_read": pgx.RepeatableRead,
		"serializable":    pgx.Serializable,
	} {
		cfg, err := Load(env(map[string]string{"DATABASE_URL": "x", "TX_ISOLATION": in}))
		if err != nil {
			t.Fatalf("TX_ISOLATION=%q: %v", in, err)
		}
		if cfg.TxIsolation != want {
			t.Errorf("TX_ISOLATION=%q: got %v, want %v", in, cfg.TxIsolation, want)
		}
	}
}

func TestLoadReportsEveryError(t *testing.T) {
	_, err := Load(env(map[string]string{
		"LOG_LEVEL":           "loud",
		"LOG_FORMAT":          "xml",
		"DB_MAX_CONNS":        "0",
		"TX_ISOLATION":        "snapshot",
		"TX_MAX_ATTEMPTS":     "many",
		"IDEMPOTENCY_TTL":     "0s",
		"REQUEST_TIMEOUT":     "-1s",
		"MIGRATE_ON_START":    "maybe",
		"TX_RETRY_BASE_DELAY": "nope",
	}))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{
		"DATABASE_URL is required",
		"LOG_LEVEL", "LOG_FORMAT", "DB_MAX_CONNS", "TX_ISOLATION", "TX_MAX_ATTEMPTS",
		"IDEMPOTENCY_TTL", "REQUEST_TIMEOUT", "MIGRATE_ON_START", "TX_RETRY_BASE_DELAY",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s:\n%v", want, err)
		}
	}
}

func TestLoadCrossFieldValidation(t *testing.T) {
	_, err := Load(env(map[string]string{
		"DATABASE_URL":        "x",
		"DB_MAX_CONNS":        "5",
		"DB_MIN_CONNS":        "6",
		"TX_RETRY_BASE_DELAY": "2s",
		"TX_RETRY_MAX_DELAY":  "1s",
		"IDEMPOTENCY_TTL":     "500ms",
	}))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"DB_MIN_CONNS", "TX_RETRY_BASE_DELAY", "IDEMPOTENCY_TTL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
}

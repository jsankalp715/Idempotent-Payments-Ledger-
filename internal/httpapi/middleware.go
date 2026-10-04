package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"
	"time"
)

type loggerKey struct{}

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// statusWriter records the status code and size of a response.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// withRequestContext assigns a request id (reusing a well-formed X-Request-ID
// from the client), attaches a request-scoped logger, applies the request
// timeout, recovers panics and writes one access-log line per request.
func (s *server) withRequestContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := r.Header.Get("X-Request-ID")
		if !requestIDPattern.MatchString(id) {
			id = newRequestID()
		}
		w.Header().Set("X-Request-ID", id)
		logger := s.logger.With("request_id", id)

		ctx := context.WithValue(r.Context(), loggerKey{}, logger)
		if s.requestTimeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, s.requestTimeout)
			defer cancel()
		}
		sw := &statusWriter{ResponseWriter: w}

		defer func() {
			if p := recover(); p != nil {
				if p == http.ErrAbortHandler {
					panic(p)
				}
				logger.ErrorContext(ctx, "panic while serving request", "panic", p, "stack", string(debug.Stack()))
				if sw.status == 0 {
					writeError(sw, http.StatusInternalServerError, "internal_error", "internal server error")
				}
			}
			level := slog.LevelInfo
			switch {
			case r.URL.Path == "/healthz" && sw.status == http.StatusOK:
				level = slog.LevelDebug
			case sw.status >= 500:
				level = slog.LevelWarn
			}
			logger.Log(ctx, level, "http request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", sw.status,
				"bytes", sw.bytes,
				"duration_ms", float64(time.Since(start).Microseconds())/1000,
				"idempotency_key", r.Header.Get("Idempotency-Key"),
				"replayed", sw.Header().Get("Idempotent-Replayed") == "true",
			)
		}()
		next.ServeHTTP(sw, r.WithContext(ctx))
	})
}

func newRequestID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

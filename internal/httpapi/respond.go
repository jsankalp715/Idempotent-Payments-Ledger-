package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"

	"github.com/jsankalp715/Idempotent-Payments-Ledger-/internal/idempotency"
	"github.com/jsankalp715/Idempotent-Payments-Ledger-/internal/ledger"
	"github.com/jsankalp715/Idempotent-Payments-Ledger-/internal/postgres"
)

// maxBodyBytes bounds request bodies; real requests are a few hundred bytes.
const maxBodyBytes = 64 << 10

// errorBody is the envelope of every error response.
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string              `json:"code"`
	Message string              `json:"message"`
	Fields  []ledger.FieldError `json:"fields,omitempty"`
}

// renderJSON encodes v as the exact bytes sent (and, for idempotent requests,
// stored and replayed).
func renderJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		// Only our own response types are rendered; failing here is a bug.
		panic(fmt.Sprintf("httpapi: render response: %v", err))
	}
	return append(b, '\n')
}

func renderError(code, message string, fields []ledger.FieldError) []byte {
	return renderJSON(errorBody{Error: errorDetail{Code: code, Message: message, Fields: fields}})
}

func writeBody(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	writeBody(w, status, renderJSON(v))
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeBody(w, status, renderError(code, message, nil))
}

// writeErr maps an error from the service layer to an HTTP response.
func (s *server) writeErr(w http.ResponseWriter, r *http.Request, err error) {
	var (
		validation *ledger.ValidationError
		domain     *ledger.Error
		reqErr     *requestError
	)
	switch {
	case errors.As(err, &reqErr):
		writeError(w, reqErr.status, reqErr.code, reqErr.message)
	case errors.As(err, &validation):
		writeBody(w, http.StatusBadRequest, renderError("validation_failed", "the request is invalid", validation.Fields))
	case errors.Is(err, idempotency.ErrInvalidKey):
		writeError(w, http.StatusBadRequest, "invalid_idempotency_key", err.Error())
	case errors.Is(err, idempotency.ErrInFlight):
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusConflict, "idempotency_key_in_use",
			"a request with this Idempotency-Key is still being processed; retry later to get its result")
	case errors.Is(err, idempotency.ErrKeyReused):
		writeError(w, http.StatusUnprocessableEntity, "idempotency_key_reused",
			"this Idempotency-Key was already used with a different request")
	case errors.As(err, &domain):
		writeError(w, domainStatus(domain), domain.Code, domain.Message)
	case errors.Is(err, postgres.ErrRetriesExhausted):
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, "transaction_conflict",
			"the request conflicted with concurrent requests and was not applied; retry with the same Idempotency-Key")
	case errors.Is(err, context.DeadlineExceeded):
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, "timeout",
			"the request timed out and was not applied; retry with the same Idempotency-Key")
	case errors.Is(err, context.Canceled) && r.Context().Err() != nil:
		// The client went away; nothing was committed and nobody is listening.
		// 499 (nginx's "client closed request") only shows up in access logs.
		w.WriteHeader(499)
	default:
		requestLogger(r.Context(), s.logger).ErrorContext(r.Context(), "request failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
	}
}

// domainStatus is the HTTP status of a business-rule outcome.
func domainStatus(e *ledger.Error) int {
	switch e.Code {
	case ledger.ErrAccountNotFound.Code, ledger.ErrTransferNotFound.Code:
		return http.StatusNotFound
	default:
		return http.StatusUnprocessableEntity
	}
}

// requestError is a client error detected while reading the request.
type requestError struct {
	status  int
	code    string
	message string
}

func (e *requestError) Error() string { return e.message }

func badRequest(code, format string, args ...any) *requestError {
	return &requestError{status: http.StatusBadRequest, code: code, message: fmt.Sprintf(format, args...)}
}

// decodeJSON strictly decodes a single JSON object from the request body:
// unknown fields, trailing data, wrong types and oversized bodies are errors.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		mediaType, _, err := mime.ParseMediaType(ct)
		if err != nil || mediaType != "application/json" {
			return &requestError{status: http.StatusUnsupportedMediaType, code: "unsupported_media_type",
				message: "Content-Type must be application/json"}
		}
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return decodeError(err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return badRequest("malformed_json", "the request body must contain a single JSON object")
	}
	return nil
}

func decodeError(err error) error {
	var (
		syntaxErr *json.SyntaxError
		typeErr   *json.UnmarshalTypeError
		maxErr    *http.MaxBytesError
	)
	switch {
	case errors.Is(err, io.EOF):
		return badRequest("malformed_json", "the request body is empty")
	case errors.As(err, &maxErr):
		return &requestError{status: http.StatusRequestEntityTooLarge, code: "body_too_large",
			message: fmt.Sprintf("the request body must not exceed %d bytes", maxBodyBytes)}
	case errors.As(err, &syntaxErr), errors.Is(err, io.ErrUnexpectedEOF):
		return badRequest("malformed_json", "the request body is not valid JSON")
	case errors.As(err, &typeErr):
		if typeErr.Field == "" {
			return badRequest("malformed_json", "the request body must be a JSON object")
		}
		return &ledger.ValidationError{Fields: []ledger.FieldError{{
			Field: typeErr.Field, Message: "must be " + jsonTypeName(typeErr.Type.Kind().String()),
		}}}
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		field := strings.Trim(strings.TrimPrefix(err.Error(), "json: unknown field "), `"`)
		return &ledger.ValidationError{Fields: []ledger.FieldError{{Field: field, Message: "is not a known field"}}}
	default:
		return badRequest("malformed_json", "the request body could not be decoded")
	}
}

func jsonTypeName(kind string) string {
	switch kind {
	case "int", "int8", "int16", "int32", "int64", "uint", "uint8", "uint16", "uint32", "uint64":
		return "an integer"
	case "string":
		return "a string"
	default:
		return "a " + kind
	}
}

// requestLogger returns the request-scoped logger set by the logging middleware.
func requestLogger(ctx context.Context, fallback *slog.Logger) *slog.Logger {
	if l, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok {
		return l
	}
	return fallback
}

package httpx

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/google/uuid"
)

// Error is a failure with a defined HTTP answer. Services return it for outcomes the caller caused (not found,
// not allowed, frozen); WriteErr turns it into the standard error body.
type Error struct {
	Status  int
	Code    string
	Message string
	Details map[string]any
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func NewError(status int, code, format string, args ...any) *Error {
	return &Error{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

// ErrNotFound is also what a caller gets for something that exists in another tenant or that they are not a member
// of: nothing reveals that it exists.
func ErrNotFound(format string, args ...any) *Error {
	return NewError(http.StatusNotFound, CodeNotFound, format, args...)
}

func ErrForbidden(format string, args ...any) *Error {
	return NewError(http.StatusForbidden, CodeForbidden, format, args...)
}

func ErrFrozen() *Error {
	return NewError(http.StatusForbidden, CodeChannelFrozen, "this channel is frozen: it is read-only")
}

func ErrConflict(format string, args ...any) *Error {
	return NewError(http.StatusConflict, CodeConflict, format, args...)
}

// WriteErr answers err: *Error and FieldErrors as themselves, anything else as 500 (logged, never shown).
func WriteErr(w http.ResponseWriter, r *http.Request, log *slog.Logger, err error) {
	var apiErr *Error
	var fields FieldErrors
	switch {
	case errors.As(err, &apiErr):
		WriteErrorDetails(w, r, apiErr.Status, apiErr.Code, apiErr.Message, apiErr.Details)
	case errors.As(err, &fields):
		WriteValidation(w, r, fields)
	default:
		log.Error("request failed", "error", err, "method", r.Method, "path", r.URL.Path, "request_id", RequestID(r.Context()))
		WriteError(w, r, http.StatusInternalServerError, CodeInternal, "internal error")
	}
}

// QueryLimit reads ?limit= (1..max, default def).
func QueryLimit(r *http.Request, def, maxLimit int) (int, error) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > maxLimit {
		return 0, FieldErrors{"limit": fmt.Sprintf("must be a number between 1 and %d", maxLimit)}
	}
	return n, nil
}

// QueryInt64 reads an optional int64 query parameter (min or more). ok is false when it is absent.
func QueryInt64(r *http.Request, name string, minValue int64) (value int64, ok bool, err error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return 0, false, nil
	}
	n, perr := strconv.ParseInt(raw, 10, 64)
	if perr != nil || n < minValue {
		return 0, false, FieldErrors{name: fmt.Sprintf("must be a whole number of %d or more", minValue)}
	}
	return n, true, nil
}

// PathUUID reads a UUID path value. An id that is not a UUID can match nothing, so it is answered with 404.
func PathUUID(r *http.Request, name string) (uuid.UUID, error) {
	id, err := uuid.Parse(r.PathValue(name))
	if err != nil {
		return uuid.Nil, ErrNotFound("%s not found", name)
	}
	return id, nil
}

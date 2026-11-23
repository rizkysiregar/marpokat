package httpx

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// DefaultMaxBody is the largest JSON body handlers accept unless they ask for more.
const DefaultMaxBody = 64 << 10

// FieldErrors is a validation failure: field name -> reason. It renders as 400 validation_failed with
// details.fields (openapi responses/Validation).
type FieldErrors map[string]string

func (f FieldErrors) Error() string {
	keys := make([]string, 0, len(f))
	for k := range f {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+": "+f[k])
	}
	return "validation failed: " + strings.Join(parts, "; ")
}

// OrNil returns nil when there are no field errors, so callers can `return errs.OrNil()`.
func (f FieldErrors) OrNil() error {
	if len(f) == 0 {
		return nil
	}
	return f
}

// DecodeJSON reads a JSON object body into dst. On failure it writes the error response and returns false.
// Unknown fields are ignored so that newer clients keep working against an older server.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, DefaultMaxBody)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			WriteError(w, r, http.StatusRequestEntityTooLarge, CodePayloadTooLarge,
				fmt.Sprintf("request body is larger than %d bytes", maxErr.Limit))
		} else {
			WriteError(w, r, http.StatusBadRequest, CodeBadRequest, "request body could not be read")
		}
		return false
	}
	if ContainsNUL(raw) { // Postgres cannot store it: left alone it would surface as a 500
		WriteError(w, r, http.StatusBadRequest, CodeBadRequest, "text must not contain the NUL character")
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(dst); err != nil {
		switch {
		case errors.Is(err, io.EOF):
			WriteError(w, r, http.StatusBadRequest, CodeBadRequest, "request body is empty")
		default:
			var typeErr *json.UnmarshalTypeError
			if errors.As(err, &typeErr) && typeErr.Field != "" {
				WriteErrorDetails(w, r, http.StatusBadRequest, CodeValidation, "invalid field type",
					map[string]any{"fields": map[string]string{typeErr.Field: "must be " + typeErr.Type.String()}})
				return false
			}
			WriteError(w, r, http.StatusBadRequest, CodeBadRequest, "request body is not valid JSON")
		}
		return false
	}
	if dec.More() {
		WriteError(w, r, http.StatusBadRequest, CodeBadRequest, "request body must contain a single JSON object")
		return false
	}
	return true
}

// ContainsNUL reports whether a JSON document holds the NUL character, raw or as the escape \u0000. The scan skips
// escaped characters, so a backslash written as \\ followed by the text u0000 is not mistaken for it.
func ContainsNUL(raw []byte) bool {
	for i := 0; i < len(raw); i++ {
		switch raw[i] {
		case 0:
			return true
		case '\\':
			if i+5 < len(raw) && raw[i+1] == 'u' && bytes.EqualFold(raw[i+2:i+6], []byte("0000")) {
				return true
			}
			i++ // the character after a backslash is part of the escape
		}
	}
	return false
}

// WriteValidation writes 400 validation_failed with the field errors.
func WriteValidation(w http.ResponseWriter, r *http.Request, f FieldErrors) {
	WriteErrorDetails(w, r, http.StatusBadRequest, CodeValidation, f.Error(), map[string]any{"fields": f})
}

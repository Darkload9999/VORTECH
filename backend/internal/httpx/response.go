// Package httpx contains the HTTP plumbing shared by every API module:
// the JSON response envelope, the error format, request decoding and
// cross-cutting middleware.
package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/vortech/backend/internal/requestid"
)

// Error codes shared across modules. Module-specific codes (for example
// RANGE_CAPACITY_EXCEEDED) are declared by the owning module.
const (
	CodeBadRequest       = "BAD_REQUEST"
	CodeInvalidJSON      = "INVALID_JSON"
	CodePayloadTooLarge  = "PAYLOAD_TOO_LARGE"
	CodeUnsupportedMedia = "UNSUPPORTED_MEDIA_TYPE"
	CodeUnauthorized     = "UNAUTHORIZED"
	CodeForbidden        = "FORBIDDEN"
	CodeNotFound         = "NOT_FOUND"
	CodeMethodNotAllowed = "METHOD_NOT_ALLOWED"
	CodeConflict         = "CONFLICT"
	CodeRateLimited      = "RATE_LIMITED"
	CodeInternal         = "INTERNAL_ERROR"
	CodeUnavailable      = "SERVICE_UNAVAILABLE"
	CodeTimeout          = "REQUEST_TIMEOUT"
)

// Envelope wraps every successful JSON response.
type Envelope struct {
	Data any `json:"data"`
	Meta any `json:"meta,omitempty"`
}

// ErrorBody is the error payload returned to clients.
type ErrorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
	Details   any    `json:"details,omitempty"`
}

// ErrorEnvelope wraps every error response.
type ErrorEnvelope struct {
	Error ErrorBody `json:"error"`
}

// Error is an error that maps onto a client-facing API error. Handlers
// return it (wrapped or not) to control status, code and message; any other
// error becomes an opaque 500.
type Error struct {
	Status  int
	Code    string
	Message string
	Details any
	// Err is the underlying cause. It is logged, never sent to clients.
	Err error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Err)
	}
	return e.Code + ": " + e.Message
}

func (e *Error) Unwrap() error { return e.Err }

// NewError builds an API error.
func NewError(status int, code, message string) *Error {
	return &Error{Status: status, Code: code, Message: message}
}

// JSON writes v inside the standard success envelope.
func JSON(w http.ResponseWriter, status int, data any) {
	writeJSON(w, status, Envelope{Data: data})
}

// JSONWithMeta writes v with pagination or other metadata.
func JSONWithMeta(w http.ResponseWriter, status int, data, meta any) {
	writeJSON(w, status, Envelope{Data: data, Meta: meta})
}

// WriteError writes an error response in the standard format.
func WriteError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	writeJSON(w, status, ErrorEnvelope{Error: ErrorBody{
		Code:      code,
		Message:   message,
		RequestID: requestid.From(r.Context()),
	}})
}

// Fail translates err into an error response. *Error values are rendered
// as-is; everything else is logged and reported as an opaque 500 so internal
// details never reach clients.
func Fail(w http.ResponseWriter, r *http.Request, log *slog.Logger, err error) {
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		log.ErrorContext(r.Context(), "request failed", "error", err, "method", r.Method, "path", r.URL.Path)
		WriteError(w, r, http.StatusInternalServerError, CodeInternal, "An internal error occurred.")
		return
	}
	if apiErr.Status >= 500 {
		log.ErrorContext(r.Context(), "request failed", "error", err, "code", apiErr.Code, "method", r.Method, "path", r.URL.Path)
	}
	WriteAPIError(w, r, apiErr)
}

// WriteAPIError renders apiErr without logging it. Use it when the caller
// has already recorded the failure.
func WriteAPIError(w http.ResponseWriter, r *http.Request, apiErr *Error) {
	writeJSON(w, apiErr.Status, ErrorEnvelope{Error: ErrorBody{
		Code:      apiErr.Code,
		Message:   apiErr.Message,
		RequestID: requestid.From(r.Context()),
		Details:   apiErr.Details,
	}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	if h.Get("Cache-Control") == "" {
		h.Set("Cache-Control", "no-store")
	}
	w.WriteHeader(status)
	// Encoding errors after WriteHeader can only be client disconnects or
	// unencodable values (a programming error caught by tests).
	_ = json.NewEncoder(w).Encode(v)
}

// DecodeJSON strictly decodes a single JSON object from the request body
// into dst. Unknown fields, trailing data and oversized bodies are rejected.
// The body size limit itself is enforced by the MaxBodyBytes middleware.
func DecodeJSON(r *http.Request, dst any) error {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		mediaType := strings.TrimSpace(strings.ToLower(strings.SplitN(ct, ";", 2)[0]))
		if mediaType != "application/json" {
			return NewError(http.StatusUnsupportedMediaType, CodeUnsupportedMedia, "Content-Type must be application/json.")
		}
	}

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return decodeError(err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return NewError(http.StatusBadRequest, CodeInvalidJSON, "Request body must contain a single JSON object.")
	}
	return nil
}

func decodeError(err error) *Error {
	var (
		syntaxErr *json.SyntaxError
		typeErr   *json.UnmarshalTypeError
		maxErr    *http.MaxBytesError
	)
	switch {
	case errors.As(err, &maxErr):
		return NewError(http.StatusRequestEntityTooLarge, CodePayloadTooLarge,
			fmt.Sprintf("Request body must not exceed %d bytes.", maxErr.Limit))
	case errors.As(err, &syntaxErr), errors.Is(err, io.ErrUnexpectedEOF):
		return NewError(http.StatusBadRequest, CodeInvalidJSON, "Request body contains malformed JSON.")
	case errors.As(err, &typeErr):
		if typeErr.Field != "" {
			return NewError(http.StatusBadRequest, CodeInvalidJSON,
				fmt.Sprintf("Field %q has an invalid type.", typeErr.Field))
		}
		return NewError(http.StatusBadRequest, CodeInvalidJSON, "Request body has an invalid type.")
	case errors.Is(err, io.EOF):
		return NewError(http.StatusBadRequest, CodeInvalidJSON, "Request body must not be empty.")
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		field := strings.TrimPrefix(err.Error(), "json: unknown field ")
		return NewError(http.StatusBadRequest, CodeInvalidJSON, fmt.Sprintf("Unknown field %s.", field))
	default:
		return NewError(http.StatusBadRequest, CodeInvalidJSON, "Request body could not be decoded.")
	}
}

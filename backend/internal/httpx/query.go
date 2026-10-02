package httpx

import (
	"encoding/base64"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// CodeInvalidQuery reports a malformed query parameter.
const CodeInvalidQuery = "INVALID_QUERY"

// PageRequest is a parsed keyset-pagination request.
type PageRequest struct {
	Limit int32
	// After is the decoded cursor: the sort key of the last item already
	// returned, or "" for the first page.
	After string
}

// PageMeta is the pagination block of a list response.
type PageMeta struct {
	Limit      int32   `json:"limit"`
	NextCursor *string `json:"next_cursor"`
}

const maxCursorBytes = 256

// ParsePage reads ?limit= and ?cursor= (an opaque token from a previous
// page's next_cursor).
func ParsePage(r *http.Request, def, max int32) (PageRequest, error) {
	p := PageRequest{Limit: def}
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil || n < 1 || n > int64(max) {
			return p, NewError(http.StatusBadRequest, CodeInvalidQuery,
				"limit must be an integer between 1 and "+strconv.Itoa(int(max))+".")
		}
		p.Limit = int32(n)
	}
	if v := r.URL.Query().Get("cursor"); v != "" {
		raw, err := base64.RawURLEncoding.DecodeString(v)
		if err != nil || len(raw) == 0 || len(raw) > maxCursorBytes {
			return p, NewError(http.StatusBadRequest, CodeInvalidQuery, "cursor is invalid.")
		}
		p.After = string(raw)
	}
	return p, nil
}

// FetchLimit is the row count to request: one extra row reveals whether
// another page exists without a COUNT query.
func (p PageRequest) FetchLimit() int32 { return p.Limit + 1 }

// Paginate trims rows fetched with FetchLimit and builds the page metadata,
// using key to derive the cursor from the last returned row.
func Paginate[T any](p PageRequest, rows []T, key func(T) string) ([]T, PageMeta) {
	meta := PageMeta{Limit: p.Limit}
	if int32(len(rows)) > p.Limit {
		rows = rows[:p.Limit]
		c := base64.RawURLEncoding.EncodeToString([]byte(key(rows[len(rows)-1])))
		meta.NextCursor = &c
	}
	return rows, meta
}

// Optional returns the query parameter as a pointer, nil when absent.
func Optional(r *http.Request, name string) *string {
	v := strings.TrimSpace(r.URL.Query().Get(name))
	if v == "" {
		return nil
	}
	return &v
}

// OptionalMatching is Optional, validated against re.
func OptionalMatching(r *http.Request, name string, re *regexp.Regexp) (*string, error) {
	v := Optional(r, name)
	if v != nil && !re.MatchString(*v) {
		return nil, NewError(http.StatusBadRequest, CodeInvalidQuery, name+" has an invalid format.")
	}
	return v, nil
}

const maxSearchRunes = 100

// SearchPattern turns ?q= into a case-insensitive substring ILIKE pattern,
// escaping LIKE metacharacters so user input is matched literally.
func SearchPattern(r *http.Request) (*string, error) {
	v := Optional(r, "q")
	if v == nil {
		return nil, nil
	}
	if len([]rune(*v)) > maxSearchRunes {
		return nil, NewError(http.StatusBadRequest, CodeInvalidQuery, "q must not exceed 100 characters.")
	}
	esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(*v)
	pattern := "%" + esc + "%"
	return &pattern, nil
}

// PathUUID parses a UUID path wildcard. Malformed IDs are reported as not
// found: they cannot name an existing resource.
func PathUUID(r *http.Request, name string) (uuid.UUID, error) {
	id, err := uuid.Parse(r.PathValue(name))
	if err != nil {
		return uuid.Nil, NewError(http.StatusNotFound, CodeNotFound, "Resource not found.")
	}
	return id, nil
}

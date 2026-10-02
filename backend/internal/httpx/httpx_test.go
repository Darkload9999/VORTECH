package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/Darkload9999/VORTECH/backend/internal/requestid"
)

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

func decodeErr(t *testing.T, rec *httptest.ResponseRecorder) ErrorBody {
	t.Helper()
	var env ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("response is not an error envelope: %v: %s", err, rec.Body.String())
	}
	return env.Error
}

func TestJSONEnvelopeAndHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	JSON(rec, http.StatusCreated, map[string]string{"k": "v"})

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("missing security headers: %v", rec.Header())
	}
	if strings.TrimSpace(rec.Body.String()) != `{"data":{"k":"v"}}` {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestWriteErrorFormat(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r = r.WithContext(requestid.With(r.Context(), "req-9"))
	rec := httptest.NewRecorder()
	WriteError(rec, r, http.StatusConflict, "RANGE_CAPACITY_EXCEEDED", "No range capacity is currently available.")

	body := decodeErr(t, rec)
	if rec.Code != http.StatusConflict || body.Code != "RANGE_CAPACITY_EXCEEDED" || body.RequestID != "req-9" {
		t.Fatalf("unexpected error response %d %+v", rec.Code, body)
	}
}

func TestFailHidesInternalErrors(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	Fail(rec, r, discard(), errors.New("pq: relation \"secret_table\" does not exist"))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "secret_table") {
		t.Fatalf("internal error leaked to client: %s", rec.Body.String())
	}
	if decodeErr(t, rec).Code != CodeInternal {
		t.Fatalf("code = %s", decodeErr(t, rec).Code)
	}
}

func TestFailRendersAPIError(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	apiErr := &Error{Status: http.StatusForbidden, Code: CodeForbidden, Message: "Nope.", Err: errors.New("internal reason")}
	Fail(rec, r, discard(), errors.Join(errors.New("wrapped"), apiErr))

	body := decodeErr(t, rec)
	if rec.Code != http.StatusForbidden || body.Code != CodeForbidden || body.Message != "Nope." {
		t.Fatalf("unexpected response %d %+v", rec.Code, body)
	}
	if strings.Contains(rec.Body.String(), "internal reason") {
		t.Fatal("wrapped cause leaked to client")
	}
}

func TestRequestIDMiddleware(t *testing.T) {
	var seen string
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = requestid.From(r.Context())
	}))

	t.Run("generates when absent", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if !requestid.Valid(seen) || rec.Header().Get(requestid.Header) != seen {
			t.Fatalf("generated id %q, header %q", seen, rec.Header().Get(requestid.Header))
		}
	})
	t.Run("adopts valid incoming", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set(requestid.Header, "openresty-abc123")
		h.ServeHTTP(httptest.NewRecorder(), r)
		if seen != "openresty-abc123" {
			t.Fatalf("seen = %q", seen)
		}
	})
	t.Run("replaces malicious incoming", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set(requestid.Header, "x\" injected=\"1")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if strings.Contains(seen, "injected") || strings.Contains(rec.Header().Get(requestid.Header), "injected") {
			t.Fatalf("malicious id adopted: %q", seen)
		}
	})
}

func TestRecoverReturnsOpaque500(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	h := Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("database password is hunter2")
	}), RequestID, AccessLog(log), Recover(log))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "hunter2") || strings.Contains(rec.Body.String(), "goroutine") {
		t.Fatalf("panic details leaked: %s", rec.Body.String())
	}
	if body := decodeErr(t, rec); body.RequestID == "" {
		t.Fatal("error response missing request_id")
	}
	if !strings.Contains(logs.String(), "panic recovered") || !strings.Contains(logs.String(), `"status":500`) {
		t.Fatalf("panic not logged or access log missing 500: %s", logs.String())
	}
}

func TestRecoverRepanicsOnAbortHandler(t *testing.T) {
	h := Recover(discard())(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	defer func() {
		if rec := recover(); rec != http.ErrAbortHandler {
			t.Fatalf("expected ErrAbortHandler to propagate, got %v", rec)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}

func TestAccessLogQuietPaths(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	h := AccessLog(log, "/health")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/health", nil))
	if logs.Len() != 0 {
		t.Fatalf("successful probe should log at debug: %s", logs.String())
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/other?token=secret", nil))
	if !strings.Contains(logs.String(), `"path":"/other"`) || !strings.Contains(logs.String(), `"bytes":2`) {
		t.Fatalf("expected access log line: %s", logs.String())
	}
	if strings.Contains(logs.String(), "secret") {
		t.Fatalf("query string must not be logged: %s", logs.String())
	}
}

func TestMaxBodyBytes(t *testing.T) {
	h := MaxBodyBytes(16)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var v map[string]any
		if err := DecodeJSON(r, &v); err != nil {
			Fail(w, r, discard(), err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	t.Run("declared length too large", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"a":"`+strings.Repeat("x", 64)+`"}`)))
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d", rec.Code)
		}
	})
	t.Run("chunked body too large", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"a":"`+strings.Repeat("x", 64)+`"}`))
		r.ContentLength = -1
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusRequestEntityTooLarge || decodeErr(t, rec).Code != CodePayloadTooLarge {
			t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("within limit", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"a":1}`)))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
		}
	})
}

func TestDecodeJSON(t *testing.T) {
	type payload struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}
	tests := []struct {
		name, body, ct string
		wantStatus     int
		wantMsg        string
	}{
		{"valid", `{"name":"x","count":1}`, "application/json", 0, ""},
		{"valid with charset", `{"name":"x"}`, "application/json; charset=utf-8", 0, ""},
		{"empty", ``, "application/json", 400, "must not be empty"},
		{"malformed", `{"name":`, "application/json", 400, "malformed"},
		{"syntax", `{"name" "x"}`, "application/json", 400, "malformed"},
		{"wrong type", `{"count":"many"}`, "application/json", 400, `"count" has an invalid type`},
		{"unknown field", `{"name":"x","is_admin":true}`, "application/json", 400, `Unknown field "is_admin"`},
		{"trailing data", `{"name":"x"}{"name":"y"}`, "application/json", 400, "single JSON object"},
		{"wrong content type", `{"name":"x"}`, "text/plain", 415, "application/json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tt.body))
			r.Header.Set("Content-Type", tt.ct)
			var p payload
			err := DecodeJSON(r, &p)
			if tt.wantStatus == 0 {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			var apiErr *Error
			if !errors.As(err, &apiErr) {
				t.Fatalf("expected *Error, got %v", err)
			}
			if apiErr.Status != tt.wantStatus || !strings.Contains(apiErr.Message, tt.wantMsg) {
				t.Fatalf("got %d %q, want %d containing %q", apiErr.Status, apiErr.Message, tt.wantStatus, tt.wantMsg)
			}
		})
	}
}

func TestTimeoutSetsDeadline(t *testing.T) {
	var deadline time.Time
	var ok bool
	h := Timeout(time.Second)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadline, ok = r.Context().Deadline()
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if !ok || time.Until(deadline) > time.Second {
		t.Fatalf("expected deadline within 1s, got ok=%v deadline=%v", ok, deadline)
	}
}

func TestRouterJSONFallbacks(t *testing.T) {
	rt := NewRouter()
	rt.HandleFunc("GET /api/v1/things/{id}", func(w http.ResponseWriter, r *http.Request) {
		JSON(w, http.StatusOK, map[string]string{"id": r.PathValue("id")})
	})

	t.Run("match populates path values", func(t *testing.T) {
		rec := httptest.NewRecorder()
		rt.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/things/42", nil))
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"id":"42"`) {
			t.Fatalf("got %d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("404 is JSON", func(t *testing.T) {
		rec := httptest.NewRecorder()
		rt.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))
		if rec.Code != 404 || decodeErr(t, rec).Code != CodeNotFound {
			t.Fatalf("got %d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("405 is JSON with Allow", func(t *testing.T) {
		rec := httptest.NewRecorder()
		rt.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/things/42", nil))
		if rec.Code != 405 || decodeErr(t, rec).Code != CodeMethodNotAllowed {
			t.Fatalf("got %d %s", rec.Code, rec.Body.String())
		}
		if allow := rec.Header().Get("Allow"); !strings.Contains(allow, "GET") {
			t.Fatalf("Allow = %q", allow)
		}
	})
}

func TestClientIPResolver(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.42.0.0/16"), netip.MustParsePrefix("127.0.0.1/32")}
	res := NewClientIPResolver(trusted)

	tests := []struct {
		name, remote string
		xff          []string
		want         string
	}{
		{"untrusted peer ignores XFF", "203.0.113.9:4000", []string{"1.2.3.4"}, "203.0.113.9"},
		{"trusted peer uses XFF", "10.42.0.5:4000", []string{"198.51.100.7"}, "198.51.100.7"},
		{"spoofed leftmost entry ignored", "10.42.0.5:4000", []string{"6.6.6.6, 198.51.100.7"}, "198.51.100.7"},
		{"skips trusted hops", "127.0.0.1:4000", []string{"198.51.100.7, 10.42.1.1"}, "198.51.100.7"},
		{"multiple headers", "10.42.0.5:4000", []string{"6.6.6.6", "198.51.100.7"}, "198.51.100.7"},
		{"malformed hop stops walk", "10.42.0.5:4000", []string{"198.51.100.7, garbage"}, "10.42.0.5"},
		{"all trusted", "10.42.0.5:4000", []string{"10.42.0.9"}, "10.42.0.9"},
		{"ipv4-mapped ipv6", "[::ffff:203.0.113.9]:4000", nil, "203.0.113.9"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tt.remote
			for _, v := range tt.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if got := res.Resolve(r).String(); got != tt.want {
				t.Fatalf("Resolve() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestClientIPMiddleware(t *testing.T) {
	var got netip.Addr
	h := ClientIPMiddleware(NewClientIPResolver(nil))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = ClientIP(r.Context())
	}))
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "192.0.2.1:1234"
	h.ServeHTTP(httptest.NewRecorder(), r)
	if got.String() != "192.0.2.1" {
		t.Fatalf("ClientIP = %s", got)
	}
	if ClientIP(context.Background()).IsValid() {
		t.Fatal("empty context should yield invalid addr")
	}
}

func TestAccessLogFailingProbeIsWarnNotError(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	h := AccessLog(log, "/ready")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/ready", nil))
	if !strings.Contains(logs.String(), `"level":"WARN"`) {
		t.Fatalf("failing probe should log at WARN: %s", logs.String())
	}

	logs.Reset()
	h = AccessLog(log, "/ready")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api", nil))
	if !strings.Contains(logs.String(), `"level":"ERROR"`) {
		t.Fatalf("5xx on a normal route should log at ERROR: %s", logs.String())
	}
}

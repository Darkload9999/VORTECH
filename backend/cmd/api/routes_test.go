package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vortech/backend/internal/config"
	"github.com/vortech/backend/internal/health"
	"github.com/vortech/backend/internal/requestid"
)

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(func(k string) (string, bool) {
		if k == "DATABASE_URL" {
			return "postgres://u:p@localhost:5432/db", true
		}
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func newTestHandler(t *testing.T, dbErr error) (http.Handler, *health.Service) {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	hs := health.New(log, time.Second, 0, health.Check{
		Name: "postgres",
		Fn:   func(context.Context) error { return dbErr },
	})
	return newHandler(testConfig(t), log, hs), hs
}

func TestHealthEndpoint(t *testing.T) {
	h, _ := newTestHandler(t, errors.New("db down"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("liveness must succeed even when the database is down, got %d", rec.Code)
	}
	if strings.TrimSpace(rec.Body.String()) != `{"data":{"status":"ok"}}` {
		t.Fatalf("body = %s", rec.Body.String())
	}
	if !requestid.Valid(rec.Header().Get(requestid.Header)) {
		t.Fatal("response missing X-Request-ID")
	}
}

func TestReadyEndpoint(t *testing.T) {
	t.Run("ready", func(t *testing.T) {
		h, _ := newTestHandler(t, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/ready", nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"postgres":"up"`) {
			t.Fatalf("got %d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("dependency down", func(t *testing.T) {
		h, _ := newTestHandler(t, errors.New("db down"))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/ready", nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("got %d", rec.Code)
		}
		var env struct {
			Error struct {
				Code      string `json:"code"`
				RequestID string `json:"request_id"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		if env.Error.Code != health.CodeNotReady || env.Error.RequestID != rec.Header().Get(requestid.Header) {
			t.Fatalf("unexpected error body %s", rec.Body.String())
		}
	})
	t.Run("draining", func(t *testing.T) {
		h, hs := newTestHandler(t, nil)
		hs.SetDraining()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/ready", nil))
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), health.CodeDraining) {
			t.Fatalf("got %d %s", rec.Code, rec.Body.String())
		}
	})
}

func TestUnknownRoutesUseErrorFormat(t *testing.T) {
	h, _ := newTestHandler(t, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/does-not-exist", nil))
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `"code":"NOT_FOUND"`) {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/health", nil))
	if rec.Code != http.StatusMethodNotAllowed || !strings.Contains(rec.Body.String(), `"code":"METHOD_NOT_ALLOWED"`) {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}

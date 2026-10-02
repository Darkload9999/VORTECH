package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/Darkload9999/VORTECH/backend/internal/config"
	"github.com/Darkload9999/VORTECH/backend/internal/requestid"
)

func newJSON(buf *bytes.Buffer, level slog.Level) *slog.Logger {
	return New(buf, config.LogConfig{Level: level, Format: config.LogFormatJSON}, "test")
}

func decode(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("log line is not JSON: %v: %s", err, buf.String())
	}
	return m
}

func TestRequestIDFromContext(t *testing.T) {
	var buf bytes.Buffer
	log := newJSON(&buf, slog.LevelInfo)
	ctx := requestid.With(context.Background(), "req-123")
	log.InfoContext(ctx, "hello")

	m := decode(t, &buf)
	if m["request_id"] != "req-123" {
		t.Fatalf("request_id = %v, want req-123", m["request_id"])
	}
	if m["service"] != "test" {
		t.Fatalf("service = %v, want test", m["service"])
	}
}

func TestRequestIDSurvivesWithAndGroup(t *testing.T) {
	var buf bytes.Buffer
	log := newJSON(&buf, slog.LevelInfo).With("component", "x").WithGroup("g")
	log.InfoContext(requestid.With(context.Background(), "abc"), "hello", "k", "v")
	if !strings.Contains(buf.String(), `"request_id":"abc"`) {
		t.Fatalf("request_id missing after With/WithGroup: %s", buf.String())
	}
}

func TestSensitiveKeysRedacted(t *testing.T) {
	var buf bytes.Buffer
	log := newJSON(&buf, slog.LevelInfo)
	log.Info("auth",
		"password", "hunter2",
		"access_token", "eyJhbGciOi",
		"Authorization", "Bearer abc",
		"client_secret", "shh",
		"DATABASE_URL", "postgres://u:p@h/db",
		slog.Group("nested", slog.String("refresh_token", "rt")),
		"username", "alice",
	)
	out := buf.String()
	for _, leaked := range []string{"hunter2", "eyJhbGciOi", "Bearer abc", "shh", "u:p@h", `"rt"`} {
		if strings.Contains(out, leaked) {
			t.Errorf("log output leaks %q: %s", leaked, out)
		}
	}
	if !strings.Contains(out, `"username":"alice"`) {
		t.Errorf("non-sensitive attribute was altered: %s", out)
	}
}

func TestLevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	log := newJSON(&buf, slog.LevelWarn)
	log.Info("dropped")
	if buf.Len() != 0 {
		t.Fatalf("info record should be filtered at warn level: %s", buf.String())
	}
	log.Warn("kept")
	if buf.Len() == 0 {
		t.Fatal("warn record should be emitted")
	}
}

func TestTextFormat(t *testing.T) {
	var buf bytes.Buffer
	New(&buf, config.LogConfig{Level: slog.LevelInfo, Format: config.LogFormatText}, "svc").Info("hi")
	if !strings.Contains(buf.String(), "msg=hi") {
		t.Fatalf("expected text output, got %s", buf.String())
	}
}

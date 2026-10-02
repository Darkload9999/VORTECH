//go:build integration

package integration

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Darkload9999/VORTECH/backend/internal/database"
	"github.com/Darkload9999/VORTECH/backend/internal/database/db"
	"github.com/Darkload9999/VORTECH/backend/internal/health"
)

// TestMigrationLifecycle must run first: it starts from an empty database.
func TestMigrationLifecycle(t *testing.T) {
	ctx := context.Background()
	m, err := database.NewMigrator(testDBURL, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	// A fresh database is behind the build: strict mode must refuse it.
	if err := database.PrepareSchema(ctx, testDBURL, false, testLogger()); err == nil ||
		!strings.Contains(err.Error(), "run the migrate job") {
		t.Fatalf("expected pending-migration error on empty schema, got %v", err)
	}

	if err := m.Up(ctx); err != nil {
		t.Fatalf("up: %v", err)
	}
	if err := m.Up(ctx); err != nil {
		t.Fatalf("second up must be a no-op: %v", err)
	}
	if err := database.PrepareSchema(ctx, testDBURL, false, testLogger()); err != nil {
		t.Fatalf("schema should be current after up: %v", err)
	}

	st, err := m.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(st) == 0 {
		t.Fatal("no migrations embedded")
	}
	for _, s := range st {
		if !s.Applied {
			t.Errorf("migration %d (%s) not applied", s.Version, s.Name)
		}
	}

	// Every migration must roll back cleanly and re-apply (reversibility).
	for range st {
		if err := m.Down(ctx); err != nil {
			t.Fatalf("down: %v", err)
		}
	}
	if err := m.Down(ctx); !errors.Is(err, database.ErrNoMigrationToRollback) {
		t.Fatalf("down on empty schema: got %v, want ErrNoMigrationToRollback", err)
	}
	if err := m.Up(ctx); err != nil {
		t.Fatalf("re-apply after full rollback: %v", err)
	}
	current, target, err := m.Versions(ctx)
	if err != nil || current != target {
		t.Fatalf("versions current=%d target=%d err=%v", current, target, err)
	}
}

func TestConcurrentMigratorsAreSerialised(t *testing.T) {
	ctx := context.Background()
	errs := make(chan error, 3)
	for range 3 {
		go func() { errs <- database.PrepareSchema(ctx, testDBURL, true, testLogger()) }()
	}
	for range 3 {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent migration failed: %v", err)
		}
	}
}

func TestAuditLogInsertAndKeysetPagination(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	q := db.New(pool)

	resource := "range-" + time.Now().Format("150405.000000")
	ip := netip.MustParseAddr("203.0.113.7")
	actor := "user-123"
	for i := range 5 {
		reqID := "req-" + string(rune('a'+i))
		row, err := q.InsertAuditLog(ctx, db.InsertAuditLogParams{
			ActorType:    "user",
			ActorID:      &actor,
			Action:       "range.created",
			ResourceType: "range",
			ResourceID:   &resource,
			Result:       "success",
			RequestID:    &reqID,
			SourceIP:     &ip,
			Metadata:     []byte(`{"cpu":"2"}`),
		})
		if err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
		if row.ID.Version() != 7 {
			t.Fatalf("expected UUIDv7 primary key, got version %d", row.ID.Version())
		}
	}

	var seen []string
	params := db.ListAuditLogsParams{PageSize: 2}
	for page := 0; page < 10; page++ {
		rows, err := q.ListAuditLogs(ctx, params)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 0 {
			break
		}
		for _, r := range rows {
			if r.ResourceID != nil && *r.ResourceID == resource {
				seen = append(seen, *r.RequestID)
				if r.SourceIP == nil || *r.SourceIP != ip {
					t.Errorf("source_ip round-trip failed: %v", r.SourceIP)
				}
			}
		}
		last := rows[len(rows)-1]
		params.CursorOccurredAt, params.CursorID = &last.OccurredAt, &last.ID
	}
	if strings.Join(seen, ",") != "req-e,req-d,req-c,req-b,req-a" {
		t.Fatalf("pagination returned %v, want newest-first without gaps or duplicates", seen)
	}
}

func TestAuditLogIsAppendOnly(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	q := db.New(pool)

	row, err := q.InsertAuditLog(ctx, db.InsertAuditLogParams{
		ActorType: "system", Action: "test.append_only", ResourceType: "audit_log",
		Result: "success", Metadata: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, stmt := range []string{
		"UPDATE audit_logs SET result = 'failure' WHERE id = $1",
		"DELETE FROM audit_logs WHERE id = $1",
	} {
		_, err := pool.Exec(ctx, stmt, row.ID)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Errorf("%q: expected insufficient_privilege (42501), got %v", stmt, err)
		}
	}
}

func TestAuditLogConstraints(t *testing.T) {
	ctx := context.Background()
	q := db.New(migratedPool(t))
	anon := "someone"

	tests := []struct {
		name string
		p    db.InsertAuditLogParams
	}{
		{"bad action format", db.InsertAuditLogParams{ActorType: "user", Action: "Range Created", ResourceType: "range", Result: "success", Metadata: []byte(`{}`)}},
		{"unknown actor type", db.InsertAuditLogParams{ActorType: "root", Action: "range.created", ResourceType: "range", Result: "success", Metadata: []byte(`{}`)}},
		{"anonymous with id", db.InsertAuditLogParams{ActorType: "anonymous", ActorID: &anon, Action: "authz.denied", ResourceType: "range", Result: "denied", Metadata: []byte(`{}`)}},
		{"unknown result", db.InsertAuditLogParams{ActorType: "user", Action: "range.created", ResourceType: "range", Result: "maybe", Metadata: []byte(`{}`)}},
		{"metadata not object", db.InsertAuditLogParams{ActorType: "user", Action: "range.created", ResourceType: "range", Result: "success", Metadata: []byte(`[1,2]`)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := q.InsertAuditLog(ctx, tt.p)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
				t.Fatalf("expected check_violation (23514), got %v", err)
			}
		})
	}
}

func TestStatementTimeoutEnforced(t *testing.T) {
	cfg := dbConfig()
	cfg.StatementTimeout = 100 * time.Millisecond
	pool, err := database.Connect(context.Background(), cfg, "vortech-it", testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	_, err = pool.Exec(context.Background(), "SELECT pg_sleep(2)")
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "57014" {
		t.Fatalf("expected query_canceled (57014) from statement_timeout, got %v", err)
	}
}

func TestReadinessAgainstRealDatabase(t *testing.T) {
	pool := migratedPool(t)
	hs := health.New(testLogger(), 2*time.Second, 0, health.Check{Name: "postgres", Fn: database.PingCheck(pool)})

	rec := httptest.NewRecorder()
	hs.Ready(rec, httptest.NewRequest(http.MethodGet, "/api/v1/ready", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("ready = %d %s", rec.Code, rec.Body.String())
	}

	pool.Close()
	rec = httptest.NewRecorder()
	hs.Ready(rec, httptest.NewRequest(http.MethodGet, "/api/v1/ready", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("ready after pool close = %d, want 503", rec.Code)
	}
}

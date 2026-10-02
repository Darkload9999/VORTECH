// Package audit records security-sensitive operations in the append-only
// audit_logs table.
//
// Entries never contain credentials or tokens: Metadata must only carry
// identifiers and non-sensitive facts.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/Darkload9999/VORTECH/backend/internal/database/db"
	"github.com/Darkload9999/VORTECH/backend/internal/httpx"
	"github.com/Darkload9999/VORTECH/backend/internal/requestid"
)

// ActorType identifies who performed an action.
type ActorType string

// Actor types (mirrors the audit_logs_actor_type_chk constraint).
const (
	ActorUser      ActorType = "user"
	ActorSystem    ActorType = "system"
	ActorService   ActorType = "service"
	ActorAnonymous ActorType = "anonymous"
)

// Result is the outcome of an audited action.
type Result string

// Results (mirrors the audit_logs_result_chk constraint).
const (
	ResultSuccess Result = "success"
	ResultFailure Result = "failure"
	ResultDenied  Result = "denied"
)

// Entry is one audit record.
type Entry struct {
	ActorType    ActorType
	ActorID      string
	Action       string // dotted lowercase, e.g. range.created
	ResourceType string
	ResourceID   string
	Result       Result
	Metadata     map[string]any
}

// Insert writes e using q, which may be bound to a transaction so the audit
// record commits atomically with the change it describes. The request ID
// and client IP are taken from ctx.
func Insert(ctx context.Context, q *db.Queries, e Entry) error {
	meta := e.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("encode audit metadata: %w", err)
	}

	p := db.InsertAuditLogParams{
		ActorType:    string(e.ActorType),
		ActorID:      optional(e.ActorID),
		Action:       e.Action,
		ResourceType: e.ResourceType,
		ResourceID:   optional(e.ResourceID),
		Result:       string(e.Result),
		RequestID:    optional(requestid.From(ctx)),
		Metadata:     metaJSON,
	}
	if ip := httpx.ClientIP(ctx); ip.IsValid() {
		p.SourceIP = &ip
	}
	if _, err := q.InsertAuditLog(ctx, p); err != nil {
		return fmt.Errorf("insert audit log %s: %w", e.Action, err)
	}
	return nil
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Recorder writes audit entries outside of a business transaction, e.g.
// for authorization denials.
type Recorder struct {
	q   *db.Queries
	log *slog.Logger
}

// NewRecorder returns a Recorder writing through q.
func NewRecorder(q *db.Queries, log *slog.Logger) *Recorder {
	return &Recorder{q: q, log: log}
}

// Record writes e. It is detached from the caller's cancellation, so a client
// disconnecting cannot suppress its own audit trail, but it is bounded by a
// short timeout. Failures are logged rather than failing the request.
func (r *Recorder) Record(ctx context.Context, e Entry) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := Insert(ctx, r.q, e); err != nil {
		r.log.ErrorContext(ctx, "audit record failed", "action", e.Action, "error", err)
	}
}

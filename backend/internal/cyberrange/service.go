package cyberrange

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Darkload9999/VORTECH/backend/internal/audit"
	"github.com/Darkload9999/VORTECH/backend/internal/auth"
	"github.com/Darkload9999/VORTECH/backend/internal/database/db"
	"github.com/Darkload9999/VORTECH/backend/internal/httpx"
	"github.com/Darkload9999/VORTECH/backend/internal/jobs"
)

// API error codes.
const (
	CodeUnknownTemplate   = "UNKNOWN_RANGE_TEMPLATE"
	CodeRangeLimit        = "RANGE_LIMIT_REACHED"
	CodeRangeNotFound     = "RANGE_NOT_FOUND"
	CodeRangeNotStoppable = "RANGE_NOT_STOPPABLE"
)

// Template is a range template as listed to players.
type Template struct {
	Slug        string    `json:"slug"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	TTLSeconds  int32     `json:"ttl_seconds"`
	Resources   Resources `json:"resources"`
	Workloads   []string  `json:"workloads"`
	Terminal    string    `json:"terminal"`
}

// Service is the API side of range management. It never talks to
// Kubernetes: it records intent and jobs, and the worker acts on them.
type Service struct {
	pool *pgxpool.Pool
	q    *db.Queries
}

// NewService returns a Service.
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool, q: db.New(pool)}
}

// Templates lists the active scenario's range templates.
func (s *Service) Templates(ctx context.Context) ([]Template, error) {
	rows, err := s.q.ListActiveRangeTemplates(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Template, 0, len(rows))
	for _, r := range rows {
		var spec Spec
		if err := json.Unmarshal(r.Spec, &spec); err != nil {
			return nil, fmt.Errorf("decode template %s: %w", r.Slug, err)
		}
		t := Template{
			Slug: r.Slug, Name: r.Name, Description: r.Description, TTLSeconds: r.TtlSeconds,
			Resources: Resources{CPUMillis: int64(r.CpuMillis), MemoryMiB: int64(r.MemoryMib), StorageMiB: int64(r.StorageMib)},
			Workloads: make([]string, len(spec.Workloads)),
		}
		for i, w := range spec.Workloads {
			t.Workloads[i] = w.Name
		}
		if ws, ok := spec.TerminalWorkload(); ok {
			t.Terminal = ws.Name
		}
		out = append(out, t)
	}
	return out, nil
}

// Create records a REQUESTED range for the caller and queues admission.
// The database guarantees at most one live range per player.
func (s *Service) Create(ctx context.Context, p *auth.Principal, templateSlug string) (View, error) {
	tpl, err := s.q.GetActiveRangeTemplateBySlug(ctx, templateSlug)
	if errors.Is(err, pgx.ErrNoRows) {
		return View{}, httpx.NewError(http.StatusUnprocessableEntity, CodeUnknownTemplate, "No active range template has that slug.")
	}
	if err != nil {
		return View{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return View{}, err
	}

	var v View
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		r, err := q.CreateRange(ctx, db.CreateRangeParams{
			ID: id, PlayerID: *p.PlayerID, TemplateID: tpl.ID, Namespace: NamespaceFor(id),
			CpuMillis: tpl.CpuMillis, MemoryMib: tpl.MemoryMib, StorageMib: tpl.StorageMib,
		})
		if err != nil {
			var pg *pgconn.PgError
			if errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == "ranges_one_live_per_player_idx" {
				return httpx.NewError(http.StatusConflict, CodeRangeLimit, "You already have a live range. Destroy it before requesting another.")
			}
			return fmt.Errorf("create range: %w", err)
		}
		if err := q.InsertRangeTransition(ctx, db.InsertRangeTransitionParams{
			RangeID: id, ToState: string(Requested), Reason: "requested by player", Actor: ActorUser(p.UserID),
		}); err != nil {
			return fmt.Errorf("record transition: %w", err)
		}
		v = ViewOf(r, tpl.Slug, tpl.Name)
		if err := Announce(ctx, tx, *p.PlayerID, v); err != nil {
			return err
		}
		if _, err := jobs.Enqueue(ctx, q, tx, JobAdmit, map[string]any{}, jobs.Options{}); err != nil {
			return err
		}
		return audit.Insert(ctx, q, audit.Entry{
			ActorType: audit.ActorUser, ActorID: p.UserID.String(),
			Action: "range.created", ResourceType: "range", ResourceID: id.String(), Result: audit.ResultSuccess,
			Metadata: map[string]any{"template": tpl.Slug, "cpu_millis": tpl.CpuMillis, "memory_mib": tpl.MemoryMib},
		})
	})
	return v, err
}

// Get returns a range visible to p: its own, or any for administrators.
// Other players' ranges are indistinguishable from missing ones.
func (s *Service) Get(ctx context.Context, p *auth.Principal, id uuid.UUID) (View, error) {
	row, err := s.visible(ctx, s.q, p, id)
	if err != nil {
		return View{}, err
	}
	return ViewOf(row.Range, row.TemplateSlug, row.TemplateName), nil
}

func (s *Service) visible(ctx context.Context, q *db.Queries, p *auth.Principal, id uuid.UUID) (db.GetRangeViewRow, error) {
	row, err := q.GetRangeView(ctx, id)
	notFound := httpx.NewError(http.StatusNotFound, CodeRangeNotFound, "Range not found.")
	if errors.Is(err, pgx.ErrNoRows) {
		return row, notFound
	}
	if err != nil {
		return row, err
	}
	own := p.PlayerID != nil && *p.PlayerID == row.Range.PlayerID
	if !own && !p.Can(auth.PermPlatformAdminister) {
		return row, notFound
	}
	return row, nil
}

// List returns the caller's ranges, newest first.
func (s *Service) List(ctx context.Context, playerID uuid.UUID, before *uuid.UUID, limit int32) ([]View, error) {
	rows, err := s.q.ListPlayerRanges(ctx, db.ListPlayerRangesParams{PlayerID: playerID, BeforeID: before, PageSize: limit})
	if err != nil {
		return nil, err
	}
	out := make([]View, len(rows))
	for i, r := range rows {
		out[i] = ViewOf(r.Range, r.TemplateSlug, r.TemplateName)
	}
	return out, nil
}

// Destroy moves a range to STOPPING and queues its teardown. It is
// idempotent: a range already stopping or destroyed is returned as is.
func (s *Service) Destroy(ctx context.Context, p *auth.Principal, id uuid.UUID) (View, error) {
	for attempt := 0; ; attempt++ {
		var v View
		err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			q := db.New(tx)
			row, err := s.visible(ctx, q, p, id)
			if err != nil {
				return err
			}
			cur := State(row.Range.State)
			switch {
			case cur == Stopping || cur == Destroyed:
				v = ViewOf(row.Range, row.TemplateSlug, row.TemplateName)
				return nil
			case !cur.Stoppable():
				return httpx.NewError(http.StatusConflict, CodeRangeNotStoppable, "The range cannot be stopped in its current state.")
			}
			if v, err = Transition(ctx, tx, id, cur, Stopping, "destroy requested", ActorUser(p.UserID)); err != nil {
				return err
			}
			if _, err := jobs.Enqueue(ctx, q, tx, JobDestroy, RangeJob{RangeID: id}, jobs.Options{}); err != nil {
				return err
			}
			return audit.Insert(ctx, q, audit.Entry{
				ActorType: audit.ActorUser, ActorID: p.UserID.String(),
				Action: "range.destroy_requested", ResourceType: "range", ResourceID: id.String(), Result: audit.ResultSuccess,
				Metadata: map[string]any{"from_state": cur},
			})
		})
		// The worker may change the state between our read and the
		// compare-and-set; re-read and try again.
		if errors.Is(err, ErrStateChanged) && attempt < 3 {
			continue
		}
		return v, err
	}
}

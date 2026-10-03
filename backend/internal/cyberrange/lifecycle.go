package cyberrange

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Darkload9999/VORTECH/backend/internal/database/db"
	"github.com/Darkload9999/VORTECH/backend/internal/event"
	"github.com/Darkload9999/VORTECH/backend/internal/notification"
)

// Job kinds handled by the worker.
const (
	// JobAdmit runs the capacity scheduler (payload {}).
	JobAdmit = "range.admit"
	// JobProvision creates a range's namespace and waits until it is ready.
	JobProvision = "range.provision"
	// JobDestroy deletes a range's namespace.
	JobDestroy = "range.destroy"
)

// RangeJob is the payload of provision and destroy jobs.
type RangeJob struct {
	RangeID uuid.UUID `json:"range_id"`
}

// ActorWorker records transitions made by the worker.
const ActorWorker = "worker"

// ActorUser records transitions requested by a user.
func ActorUser(userID uuid.UUID) string { return "user:" + userID.String() }

// EventType is the real-time event emitted on entering s, e.g. range.ready.
func EventType(s State) string { return "range." + strings.ToLower(string(s)) }

// ErrStateChanged means a compare-and-set transition lost a race: the
// range was no longer in the expected state.
var ErrStateChanged = errors.New("range state changed concurrently")

// TemplateRef names a range's template.
type TemplateRef struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// View is the API and event representation of a range. The Kubernetes
// namespace is deliberately not exposed.
type View struct {
	ID            uuid.UUID   `json:"id"`
	Template      TemplateRef `json:"template"`
	State         State       `json:"state"`
	Resources     Resources   `json:"resources"`
	FailureReason *string     `json:"failure_reason"`
	ExpiresAt     *time.Time  `json:"expires_at"`
	ReadyAt       *time.Time  `json:"ready_at"`
	DestroyedAt   *time.Time  `json:"destroyed_at"`
	CreatedAt     time.Time   `json:"created_at"`
	UpdatedAt     time.Time   `json:"updated_at"`
}

// ViewOf converts a database row.
func ViewOf(r db.Range, templateSlug, templateName string) View {
	utc := func(t *time.Time) *time.Time {
		if t == nil {
			return nil
		}
		u := t.UTC()
		return &u
	}
	return View{
		ID:       r.ID,
		Template: TemplateRef{Slug: templateSlug, Name: templateName},
		State:    State(r.State),
		Resources: Resources{
			CPUMillis: int64(r.RequestedCpuMillis), MemoryMiB: int64(r.RequestedMemoryMib), StorageMiB: int64(r.RequestedStorageMib),
		},
		FailureReason: r.FailureReason,
		ExpiresAt:     utc(r.ExpiresAt),
		ReadyAt:       utc(r.ReadyAt),
		DestroyedAt:   utc(r.DestroyedAt),
		CreatedAt:     r.CreatedAt.UTC(),
		UpdatedAt:     r.UpdatedAt.UTC(),
	}
}

// Transition moves range id from → to inside tx: a compare-and-set update,
// a history row, the player's real-time event and, for outcomes the
// player must not miss, a durable notification. Events go through
// pg_notify, so they are delivered only if tx commits.
func Transition(ctx context.Context, tx pgx.Tx, id uuid.UUID, from, to State, reason, actor string) (View, error) {
	if err := CheckTransition(from, to); err != nil {
		return View{}, err
	}
	q := db.New(tx)
	if _, err := q.TransitionRange(ctx, db.TransitionRangeParams{ID: id, FromState: string(from), ToState: string(to), Reason: reason}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return View{}, ErrStateChanged
		}
		return View{}, fmt.Errorf("transition range %s: %w", id, err)
	}
	fs := string(from)
	if err := q.InsertRangeTransition(ctx, db.InsertRangeTransitionParams{RangeID: id, FromState: &fs, ToState: string(to), Reason: reason, Actor: actor}); err != nil {
		return View{}, fmt.Errorf("record transition: %w", err)
	}
	row, err := q.GetRangeView(ctx, id)
	if err != nil {
		return View{}, fmt.Errorf("load range: %w", err)
	}
	v := ViewOf(row.Range, row.TemplateSlug, row.TemplateName)
	if err := Announce(ctx, tx, row.Range.PlayerID, v); err != nil {
		return View{}, err
	}
	if err := notify(ctx, tx, q, row.UserID, row.Range.PlayerID, v); err != nil {
		return View{}, err
	}
	return v, nil
}

// Announce emits the player's real-time event for v's current state.
func Announce(ctx context.Context, tx pgx.Tx, playerID uuid.UUID, v View) error {
	e, err := event.New(EventType(v.State), event.PlayerTopic(playerID), v)
	if err != nil {
		return err
	}
	return event.Notify(ctx, tx, e)
}

func notify(ctx context.Context, tx pgx.Tx, q *db.Queries, userID, playerID uuid.UUID, v View) error {
	var typ, title, body string
	switch v.State {
	case Ready:
		typ, title = notification.TypeRangeReady, "Your range is ready"
		body = fmt.Sprintf("%s is up. Your workstation terminal is available.", v.Template.Name)
	case Failed:
		typ, title = notification.TypeRangeFailed, "Your range could not be started"
		body = fmt.Sprintf("%s failed to start and is being cleaned up. You can request it again.", v.Template.Name)
	case Expired:
		typ, title = notification.TypeRangeExpired, "Your range has expired"
		body = fmt.Sprintf("%s reached its time limit and is being shut down.", v.Template.Name)
	default:
		return nil
	}
	_, e, err := notification.Create(ctx, q, userID, event.PlayerTopic(playerID), typ, title, body, map[string]any{"range_id": v.ID})
	if err != nil {
		return err
	}
	return event.Notify(ctx, tx, e)
}

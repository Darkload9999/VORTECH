package company

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Darkload9999/VORTECH/backend/internal/database/db"
	"github.com/Darkload9999/VORTECH/backend/internal/httpx"
)

// CodeWorldNotActive is returned when no scenario world is published.
const CodeWorldNotActive = "WORLD_NOT_ACTIVE"

// ErrNoActiveWorld means no scenario is active (run `scenario import -activate`).
var ErrNoActiveWorld = &httpx.Error{
	Status:  http.StatusServiceUnavailable,
	Code:    CodeWorldNotActive,
	Message: "No scenario world is currently active.",
}

// Company is the active fictional organisation.
type Company = db.GetActiveCompanyRow

// Directory resolves the company whose world the platform serves. The
// result is cached briefly: it changes only when a scenario is activated.
type Directory struct {
	q   *db.Queries
	ttl time.Duration
	now func() time.Time

	mu       sync.Mutex
	cached   *Company
	cachedAt time.Time
}

// NewDirectory returns a Directory caching for ttl.
func NewDirectory(q *db.Queries, ttl time.Duration) *Directory {
	return &Directory{q: q, ttl: ttl, now: time.Now}
}

// Active returns the active company or ErrNoActiveWorld.
func (d *Directory) Active(ctx context.Context) (Company, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.cachedAt.IsZero() && d.now().Sub(d.cachedAt) < d.ttl {
		if d.cached == nil {
			return Company{}, ErrNoActiveWorld
		}
		return *d.cached, nil
	}

	c, err := d.q.GetActiveCompany(ctx)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		d.cached, d.cachedAt = nil, d.now()
		return Company{}, ErrNoActiveWorld
	case err != nil:
		return Company{}, fmt.Errorf("load active company: %w", err)
	}
	d.cached, d.cachedAt = &c, d.now()
	return c, nil
}

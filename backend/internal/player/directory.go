package player

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/singleflight"

	"github.com/vortech/backend/internal/audit"
	"github.com/vortech/backend/internal/auth"
	"github.com/vortech/backend/internal/database/db"
)

const (
	statusSuspended = "suspended"
	// maxCacheEntries bounds memory used by the identity cache.
	maxCacheEntries = 50_000
	// resolveTimeout bounds the shared database work for one identity.
	resolveTimeout = 5 * time.Second
)

// Directory maps Keycloak subjects to internal users and players. It
// implements auth.IdentityResolver.
//
// The first request of a new account creates its user (and, for holders of
// the PLAYER role, its player) together with the audit record in one
// transaction. Results are cached in memory for a short TTL so steady-state
// requests cost no database round trip; the TTL also bounds how long an
// account suspension takes to apply. The cache moves to Valkey once the API
// runs as more than one replica.
type Directory struct {
	pool *pgxpool.Pool
	log  *slog.Logger
	ttl  time.Duration
	now  func() time.Time

	flight singleflight.Group

	mu    sync.Mutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	ident   auth.Identity
	expires time.Time
}

// NewDirectory returns a Directory; ttl 0 disables caching.
func NewDirectory(pool *pgxpool.Pool, log *slog.Logger, ttl time.Duration) *Directory {
	return &Directory{
		pool:  pool,
		log:   log,
		ttl:   ttl,
		now:   time.Now,
		cache: make(map[string]cacheEntry),
	}
}

// Resolve implements auth.IdentityResolver.
func (d *Directory) Resolve(ctx context.Context, c *auth.Claims, roles auth.RoleSet) (auth.Identity, error) {
	wantPlayer := roles.Has(auth.RolePlayer)
	// The PLAYER role is part of the key: gaining it must provision a player
	// immediately instead of waiting for the cache entry to expire.
	key := c.Subject + "|" + strconv.FormatBool(wantPlayer)
	if ident, ok := d.cached(key); ok {
		return ident, nil
	}

	// Concurrent first requests for the same account share one round trip.
	// The shared work is detached from any single caller's cancellation.
	v, err, _ := d.flight.Do(key, func() (any, error) {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), resolveTimeout)
		defer cancel()
		ident, err := d.sync(sctx, c, wantPlayer)
		if err != nil {
			return auth.Identity{}, err
		}
		d.store(key, ident)
		return ident, nil
	})
	if err != nil {
		return auth.Identity{}, fmt.Errorf("resolve identity: %w", err)
	}
	return v.(auth.Identity), nil
}

// Forget drops cached mappings for subject, e.g. after an admin suspends
// the account, so the change applies immediately on this instance.
func (d *Directory) Forget(subject string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.cache, subject+"|true")
	delete(d.cache, subject+"|false")
}

func (d *Directory) sync(ctx context.Context, c *auth.Claims, wantPlayer bool) (auth.Identity, error) {
	var ident auth.Identity
	err := pgx.BeginFunc(ctx, d.pool, func(tx pgx.Tx) error {
		q := db.New(tx)

		username := truncate(c.PreferredUsername, 255)
		if username == "" {
			username = c.Subject
		}
		u, err := q.UpsertUserFromToken(ctx, db.UpsertUserFromTokenParams{
			KeycloakSubject: c.Subject,
			Username:        username,
			Email:           optional(truncate(c.Email, 320)),
			EmailVerified:   c.EmailVerified,
			FullName:        optional(truncate(c.Name, 255)),
		})
		if err != nil {
			return fmt.Errorf("upsert user: %w", err)
		}
		ident = auth.Identity{UserID: u.ID, Suspended: u.Status == statusSuspended}

		if u.Inserted {
			d.log.InfoContext(ctx, "user created from identity provider", "user_id", u.ID, "username", username)
			if err := audit.Insert(ctx, q, audit.Entry{
				ActorType:    audit.ActorUser,
				ActorID:      u.ID.String(),
				Action:       "identity.user_created",
				ResourceType: "user",
				ResourceID:   u.ID.String(),
				Result:       audit.ResultSuccess,
				Metadata: map[string]any{
					"keycloak_subject": c.Subject,
					"username":         username,
				},
			}); err != nil {
				return err
			}
		}

		if !wantPlayer {
			return nil
		}
		playerID, err := d.ensurePlayer(ctx, q, u.ID, username)
		if err != nil {
			return err
		}
		ident.PlayerID = &playerID
		return nil
	})
	return ident, err
}

func (d *Directory) ensurePlayer(ctx context.Context, q *db.Queries, userID uuid.UUID, username string) (uuid.UUID, error) {
	id, err := q.InsertPlayerIfMissing(ctx, db.InsertPlayerIfMissingParams{
		UserID:      userID,
		DisplayName: truncate(username, 64),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		id, err = q.GetPlayerIDByUserID(ctx, userID)
		if err != nil {
			return uuid.Nil, fmt.Errorf("load player: %w", err)
		}
		return id, nil
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("create player: %w", err)
	}
	d.log.InfoContext(ctx, "player profile created", "user_id", userID, "player_id", id)
	err = audit.Insert(ctx, q, audit.Entry{
		ActorType:    audit.ActorUser,
		ActorID:      userID.String(),
		Action:       "identity.player_created",
		ResourceType: "player",
		ResourceID:   id.String(),
		Result:       audit.ResultSuccess,
	})
	return id, err
}

func (d *Directory) cached(key string) (auth.Identity, bool) {
	if d.ttl <= 0 {
		return auth.Identity{}, false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.cache[key]
	if !ok || d.now().After(e.expires) {
		return auth.Identity{}, false
	}
	return e.ident, true
}

func (d *Directory) store(key string, ident auth.Identity) {
	if d.ttl <= 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.cache) >= maxCacheEntries {
		d.evictLocked()
	}
	d.cache[key] = cacheEntry{ident: ident, expires: d.now().Add(d.ttl)}
}

// evictLocked drops expired entries and, if the cache is still full, an
// arbitrary tenth of it (Go map iteration order is randomised).
func (d *Directory) evictLocked() {
	now := d.now()
	for k, e := range d.cache {
		if now.After(e.expires) {
			delete(d.cache, k)
		}
	}
	excess := len(d.cache) - maxCacheEntries*9/10
	for k := range d.cache {
		if excess <= 0 {
			break
		}
		delete(d.cache, k)
		excess--
	}
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// truncate shortens s to at most n runes without splitting a character.
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vortech/backend/internal/audit"
	"github.com/vortech/backend/internal/auth"
	"github.com/vortech/backend/internal/auth/authtest"
	"github.com/vortech/backend/internal/database/db"
	"github.com/vortech/backend/internal/httpx"
	"github.com/vortech/backend/internal/player"
)

func claimsFor(subject, username string) *auth.Claims {
	c := &auth.Claims{PreferredUsername: username, Email: username + "@vortech.test", EmailVerified: true, Name: "Test " + username}
	c.Subject = subject
	return c
}

func countRows(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func TestDirectoryProvisionsUserPlayerAndAudit(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	d := player.NewDirectory(pool, testLogger(), 0)
	sub := uuid.NewString()
	players := auth.ParseRoles([]string{"PLAYER"})

	first, err := d.Resolve(ctx, claimsFor(sub, "neo"), players)
	if err != nil {
		t.Fatal(err)
	}
	if first.PlayerID == nil || first.Suspended {
		t.Fatalf("unexpected identity %+v", first)
	}
	second, err := d.Resolve(ctx, claimsFor(sub, "neo"), players)
	if err != nil {
		t.Fatal(err)
	}
	if second.UserID != first.UserID || *second.PlayerID != *first.PlayerID {
		t.Fatalf("resolution not stable: %+v vs %+v", first, second)
	}

	if n := countRows(t, pool, "SELECT count(*) FROM users WHERE keycloak_subject = $1", sub); n != 1 {
		t.Fatalf("users rows = %d", n)
	}
	for action, want := range map[string]int{"identity.user_created": 1, "identity.player_created": 1} {
		if n := countRows(t, pool, "SELECT count(*) FROM audit_logs WHERE action = $1 AND actor_id = $2", action, first.UserID.String()); n != want {
			t.Errorf("%s audit rows = %d, want %d", action, n, want)
		}
	}
}

func TestDirectorySyncsProfileChanges(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	d := player.NewDirectory(pool, testLogger(), 0)
	sub := uuid.NewString()

	id, err := d.Resolve(ctx, claimsFor(sub, "trinity"), auth.RoleSet{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Resolve(ctx, claimsFor(sub, "trinity2"), auth.RoleSet{}); err != nil {
		t.Fatal(err)
	}
	var username, email string
	if err := pool.QueryRow(ctx, "SELECT username, email FROM users WHERE id = $1", id.UserID).Scan(&username, &email); err != nil {
		t.Fatal(err)
	}
	if username != "trinity2" || email != "trinity2@vortech.test" {
		t.Fatalf("profile not synced: %s %s", username, email)
	}
}

func TestDirectoryPlayerOnlyWithPlayerRole(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	d := player.NewDirectory(pool, testLogger(), time.Minute)
	sub := uuid.NewString()

	id, err := d.Resolve(ctx, claimsFor(sub, "instructor-only"), auth.ParseRoles([]string{"INSTRUCTOR"}))
	if err != nil {
		t.Fatal(err)
	}
	if id.PlayerID != nil {
		t.Fatal("no player profile expected without the PLAYER role")
	}
	// Gaining the role must take effect immediately despite the cache.
	id, err = d.Resolve(ctx, claimsFor(sub, "instructor-only"), auth.ParseRoles([]string{"INSTRUCTOR", "PLAYER"}))
	if err != nil {
		t.Fatal(err)
	}
	if id.PlayerID == nil {
		t.Fatal("player profile not created after PLAYER role was granted")
	}
}

func TestDirectoryConcurrentFirstLogin(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	sub := uuid.NewString()
	roles := auth.ParseRoles([]string{"PLAYER"})

	// Separate directories defeat in-process deduplication and model several
	// API replicas racing on the database.
	const n = 16
	ids := make([]auth.Identity, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids[i], errs[i] = player.NewDirectory(pool, testLogger(), 0).Resolve(ctx, claimsFor(sub, "racer"), roles)
		}()
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("resolve %d: %v", i, errs[i])
		}
		if ids[i].UserID != ids[0].UserID || *ids[i].PlayerID != *ids[0].PlayerID {
			t.Fatalf("concurrent first logins produced different identities")
		}
	}
	if c := countRows(t, pool, "SELECT count(*) FROM audit_logs WHERE action = 'identity.user_created' AND actor_id = $1", ids[0].UserID.String()); c != 1 {
		t.Fatalf("user_created audited %d times", c)
	}
	if c := countRows(t, pool, "SELECT count(*) FROM players WHERE user_id = $1", ids[0].UserID); c != 1 {
		t.Fatalf("players rows = %d", c)
	}
}

func TestDirectorySuspensionAndCacheInvalidation(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	d := player.NewDirectory(pool, testLogger(), time.Hour)
	sub := uuid.NewString()

	id, err := d.Resolve(ctx, claimsFor(sub, "smith"), auth.RoleSet{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "UPDATE users SET status = 'suspended' WHERE id = $1", id.UserID); err != nil {
		t.Fatal(err)
	}
	if cached, _ := d.Resolve(ctx, claimsFor(sub, "smith"), auth.RoleSet{}); cached.Suspended {
		t.Fatal("expected cached (stale) identity before Forget")
	}
	d.Forget(sub)
	fresh, err := d.Resolve(ctx, claimsFor(sub, "smith"), auth.RoleSet{})
	if err != nil {
		t.Fatal(err)
	}
	if !fresh.Suspended {
		t.Fatal("suspension not visible after Forget")
	}
}

// apiStack mounts real auth + player components behind the standard
// middleware, as the API does.
type apiStack struct {
	handler http.Handler
	pool    *pgxpool.Pool
}

func newAPIStack(t *testing.T, verifier auth.TokenVerifier) *apiStack {
	t.Helper()
	pool := migratedPool(t)
	q := db.New(pool)
	log := testLogger()
	authn := auth.NewAuthenticator(verifier, player.NewDirectory(pool, log, 0), audit.NewRecorder(q, log), log)

	r := httpx.NewRouter()
	r.Handle("GET /api/v1/me", authn.Protect(auth.PermProfileReadOwn, http.HandlerFunc(player.NewHandler(q, log).Me)))
	r.Handle("GET /api/v1/admin/ping", authn.Protect(auth.PermPlatformAdminister, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})))
	return &apiStack{
		pool: pool,
		handler: httpx.Chain(r,
			httpx.RequestID,
			httpx.ClientIPMiddleware(httpx.NewClientIPResolver(nil)),
			httpx.Recover(log),
		),
	}
}

func (s *apiStack) get(t *testing.T, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.RemoteAddr = "198.51.100.23:5555"
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, r)
	return rec
}

type meBody struct {
	Data player.MeResponse `json:"data"`
}

func decodeMe(t *testing.T, rec *httptest.ResponseRecorder) player.MeResponse {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /me = %d %s", rec.Code, rec.Body.String())
	}
	var b meBody
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	return b.Data
}

func fakeIssuerVerifier(t *testing.T) (*authtest.Issuer, *auth.Verifier) {
	t.Helper()
	iss := authtest.NewIssuer(t)
	ks := auth.NewKeySet(iss.JWKSURL(), testLogger())
	if err := ks.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return iss, auth.NewVerifier(ks, iss.VerifierConfig())
}

func TestMeEndToEndWithDatabase(t *testing.T) {
	iss, v := fakeIssuerVerifier(t)
	s := newAPIStack(t, v)
	sub := uuid.NewString()

	me := decodeMe(t, s.get(t, "/api/v1/me", iss.Token(t, sub, "PLAYER")))
	if me.User.Username != sub || me.Player == nil || len(me.Roles) != 1 || me.Roles[0] != auth.RolePlayer {
		t.Fatalf("unexpected /me %+v", me)
	}
	if me.Player.DisplayName == "" || me.Session.ExpiresAt.Before(time.Now()) {
		t.Fatalf("unexpected player/session %+v %+v", me.Player, me.Session)
	}

	// Another player sees only their own profile.
	other := decodeMe(t, s.get(t, "/api/v1/me", iss.Token(t, uuid.NewString(), "PLAYER")))
	if other.User.ID == me.User.ID || other.Player.ID == me.Player.ID {
		t.Fatal("/me leaked another user's profile")
	}
}

func TestAuthorizationDenialIsPersistedToAuditLog(t *testing.T) {
	iss, v := fakeIssuerVerifier(t)
	s := newAPIStack(t, v)
	sub := uuid.NewString()

	rec := s.get(t, "/api/v1/admin/ping", iss.Token(t, sub, "PLAYER"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d", rec.Code)
	}
	reqID := rec.Header().Get("X-Request-ID")

	var action, result, resource, ip, rid string
	err := s.pool.QueryRow(context.Background(), `
		SELECT a.action, a.result, a.resource_id, host(a.source_ip), a.request_id
		FROM audit_logs a JOIN users u ON a.actor_id = u.id::text
		WHERE u.keycloak_subject = $1 AND a.action = 'authz.denied'`, sub).Scan(&action, &result, &resource, &ip, &rid)
	if err != nil {
		t.Fatalf("denial not audited: %v", err)
	}
	if result != "denied" || resource != "GET /api/v1/admin/ping" || ip != "198.51.100.23" || rid != reqID {
		t.Fatalf("unexpected audit row: %s %s %s %s %s", action, result, resource, ip, rid)
	}

	// Admins pass.
	if rec := s.get(t, "/api/v1/admin/ping", iss.Token(t, uuid.NewString(), "ADMIN")); rec.Code != http.StatusNoContent {
		t.Fatalf("admin got %d", rec.Code)
	}
}

func TestSuspendedUserBlockedEndToEnd(t *testing.T) {
	iss, v := fakeIssuerVerifier(t)
	s := newAPIStack(t, v)
	sub := uuid.NewString()

	me := decodeMe(t, s.get(t, "/api/v1/me", iss.Token(t, sub, "PLAYER")))
	if _, err := s.pool.Exec(context.Background(), "UPDATE users SET status = 'suspended' WHERE id = $1", me.User.ID); err != nil {
		t.Fatal(err)
	}
	rec := s.get(t, "/api/v1/me", iss.Token(t, sub, "PLAYER"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("suspended user got %d %s", rec.Code, rec.Body.String())
	}
}

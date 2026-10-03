//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Darkload9999/VORTECH/backend/internal/audit"
	"github.com/Darkload9999/VORTECH/backend/internal/auth"
	"github.com/Darkload9999/VORTECH/backend/internal/auth/authtest"
	"github.com/Darkload9999/VORTECH/backend/internal/career"
	"github.com/Darkload9999/VORTECH/backend/internal/company"
	"github.com/Darkload9999/VORTECH/backend/internal/database/db"
	"github.com/Darkload9999/VORTECH/backend/internal/httpx"
	"github.com/Darkload9999/VORTECH/backend/internal/interaction"
	"github.com/Darkload9999/VORTECH/backend/internal/player"
	"github.com/Darkload9999/VORTECH/backend/internal/scenario"
	"github.com/Darkload9999/VORTECH/backend/internal/world"
)

type gameStack struct {
	handler http.Handler
	pool    *pgxpool.Pool
	iss     *authtest.Issuer
	careers *career.Service
}

func newGameStack(t *testing.T, limiter *interaction.Limiter) *gameStack {
	t.Helper()
	pool := migratedPool(t)
	if _, err := scenario.Import(context.Background(), pool, loadNexora(t, nexoraDir), scenario.ImportOptions{Activate: true}); err != nil {
		t.Fatal(err)
	}
	iss, v := fakeIssuerVerifier(t)
	q := db.New(pool)
	log := testLogger()
	authn := auth.NewAuthenticator(v, player.NewDirectory(pool, log, 0), audit.NewRecorder(q, log), log)
	dir := company.NewDirectory(q, 0)
	careers := career.NewService(q)
	wh := world.NewHandler(dir, q, careers, log)

	r := httpx.NewRouter()
	r.Handle("GET /api/v1/me/progress", authn.Protect(auth.PermScenarioPlay, http.HandlerFunc(career.NewHandler(careers, dir, q, log).Me)))
	r.Handle("POST /api/v1/interactions", authn.Protect(auth.PermScenarioPlay,
		http.HandlerFunc(interaction.NewHandler(interaction.NewService(pool, dir, careers, nil), limiter, log).Create)))
	r.Handle("GET /api/v1/world", authn.Protect(auth.PermWorldRead, http.HandlerFunc(wh.World)))
	r.Handle("GET /api/v1/world/zones/{id}", authn.Protect(auth.PermWorldRead, http.HandlerFunc(wh.Zone)))
	r.Handle("GET /api/v1/world/objects/{key}", authn.Protect(auth.PermWorldRead, http.HandlerFunc(wh.Object)))
	return &gameStack{handler: httpx.Chain(r, httpx.RequestID, httpx.Recover(log)), pool: pool, iss: iss, careers: careers}
}

// as is one authenticated caller.
type as struct {
	s     *gameStack
	sub   string
	roles []string
}

func (s *gameStack) player(t *testing.T) *as {
	return &as{s: s, sub: "gamer-" + uuid.NewString()[:8], roles: []string{"PLAYER"}}
}

func (a *as) do(t *testing.T, method, target string, body any, out any) int {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	r := httptest.NewRequest(method, target, rd)
	r.Header.Set("Authorization", "Bearer "+a.s.iss.Token(t, a.sub, a.roles...))
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	a.s.handler.ServeHTTP(rec, r)
	if out != nil && rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("decode: %v: %s", err, rec.Body.String())
		}
	}
	return rec.Code
}

type outcomeEnv struct {
	Data struct {
		Outcome   string         `json:"outcome"`
		Reason    *string        `json:"reason"`
		Result    map[string]any `json:"result"`
		XPAwarded int32          `json:"xp_awarded"`
		Progress  struct {
			XP                   int32           `json:"xp"`
			Level                career.Level    `json:"level"`
			LevelUp              bool            `json:"level_up"`
			CurrentZone          *career.ZoneRef `json:"current_zone"`
			NewlyAccessibleZones []string        `json:"newly_accessible_zones"`
		} `json:"progress"`
	} `json:"data"`
}

func (a *as) interact(t *testing.T, body map[string]any) outcomeEnv {
	t.Helper()
	var o outcomeEnv
	if code := a.do(t, http.MethodPost, "/api/v1/interactions", body, &o); code != 200 {
		t.Fatalf("interaction %v: status %d", body, code)
	}
	return o
}

func expectAllowed(t *testing.T, o outcomeEnv) {
	t.Helper()
	if o.Data.Outcome != "allowed" {
		t.Fatalf("expected allowed, got denied (%v)", deref(o.Data.Reason))
	}
}

func expectDenied(t *testing.T, o outcomeEnv, reason string) {
	t.Helper()
	if o.Data.Outcome != "denied" || deref(o.Data.Reason) != reason {
		t.Fatalf("expected denied %s, got %s (%v)", reason, o.Data.Outcome, deref(o.Data.Reason))
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func enter(code string, s *gameStack, t *testing.T) map[string]any {
	return map[string]any{"type": "ENTER_ZONE", "zone_id": zoneID(t, s.pool, code)}
}

func employeeID(t *testing.T, pool *pgxpool.Pool, code string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(), "SELECT id FROM employees WHERE employee_code = $1", code).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestGameplayLoop(t *testing.T) {
	s := newGameStack(t, nil)
	p := s.player(t)

	// Fresh player: intern, outside the world, floor 3 gated by rank.
	var prog struct{ Data career.ProgressView }
	if code := p.do(t, http.MethodGet, "/api/v1/me/progress", nil, &prog); code != 200 {
		t.Fatalf("progress status %d", code)
	}
	if prog.Data.XP != 0 || prog.Data.Level.Code != "INTERN" || prog.Data.CurrentZone != nil || prog.Data.NextLevel == nil || prog.Data.NextLevel.XPRemaining != 100 {
		t.Fatalf("unexpected initial progress %+v", prog.Data)
	}
	access := map[string]career.ZoneAccess{}
	for _, z := range prog.Data.Zones {
		access[z.Code] = z
	}
	if !access["HQ_LOBBY"].Accessible || access["HQ_FLOOR_3"].Accessible || access["HQ_FLOOR_3"].Reason != "INSUFFICIENT_RANK" || access["HQ_FLOOR_3"].RequiredRank != 1 {
		t.Fatalf("unexpected zone access %+v", access)
	}

	use := map[string]any{"type": "USE_WORKSTATION", "object_key": "finance_pc_04"}
	expectDenied(t, p.interact(t, use), "NOT_IN_ZONE")
	expectDenied(t, p.interact(t, enter("HQ_FLOOR_3", s, t)), "INSUFFICIENT_RANK")

	o := p.interact(t, enter("HQ_FLOOR_2", s, t))
	expectAllowed(t, o)
	if o.Data.Progress.CurrentZone == nil || o.Data.Progress.CurrentZone.Code != "HQ_FLOOR_2" {
		t.Fatalf("current zone not updated: %+v", o.Data.Progress)
	}

	// Physical → digital: using Alex's PC reveals and discovers the asset.
	o = p.interact(t, use)
	expectAllowed(t, o)
	asset := o.Data.Result["asset"].(map[string]any)
	disc := o.Data.Result["discovery"].(map[string]any)
	if asset["code"] != "HQ-FIN-PC-04" || asset["hostname"] != "FIN-PC04" || asset["ip_address"] != "10.20.30.44" || disc["new"] != true || o.Data.XPAwarded != 10 {
		t.Fatalf("unexpected workstation result %+v xp=%d", o.Data.Result, o.Data.XPAwarded)
	}
	o = p.interact(t, use)
	if o.Data.Result["discovery"].(map[string]any)["new"] != false || o.Data.XPAwarded != 0 || o.Data.Progress.XP != 10 {
		t.Fatalf("re-discovery must not award XP: %+v", o.Data)
	}

	expectDenied(t, p.interact(t, map[string]any{"type": "OPEN_DOOR", "object_key": "finance_pc_04"}), "INTERACTION_NOT_SUPPORTED")

	o = p.interact(t, map[string]any{"type": "READ_DOCUMENT", "object_key": "finance_whiteboard"})
	expectAllowed(t, o)
	if !strings.Contains(o.Data.Result["content"].(string), "Q3 CLOSE CHECKLIST") {
		t.Fatalf("document content missing: %+v", o.Data.Result)
	}

	o = p.interact(t, map[string]any{"type": "TALK_TO_NPC", "employee_id": employeeID(t, s.pool, "EMP-0018")})
	expectAllowed(t, o)
	if !strings.Contains(o.Data.Result["greeting"].(string), "slow again") {
		t.Fatalf("unexpected greeting %+v", o.Data.Result)
	}
	expectDenied(t, p.interact(t, map[string]any{"type": "TALK_TO_NPC", "employee_id": employeeID(t, s.pool, "EMP-0022")}), "NOT_IN_ZONE")

	// Locked zone contents are hidden from players but visible to staff.
	f3 := zoneID(t, s.pool, "HQ_FLOOR_3").String()
	if code := p.do(t, http.MethodGet, "/api/v1/world/zones/"+f3, nil, nil); code != 403 {
		t.Fatalf("player read locked zone: %d", code)
	}
	if code := p.do(t, http.MethodGet, "/api/v1/world/objects/eng_deploy_screen", nil, nil); code != 403 {
		t.Fatalf("player read object in locked zone: %d", code)
	}
	instructor := &as{s: s, sub: "teacher-" + uuid.NewString()[:8], roles: []string{"PLAYER", "INSTRUCTOR"}}
	if code := instructor.do(t, http.MethodGet, "/api/v1/world/zones/"+f3, nil, nil); code != 200 {
		t.Fatalf("instructor read locked zone: %d", code)
	}

	// Ranking up opens rank-gated zones.
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE player_progress SET xp = 95 WHERE player_id = (SELECT p.id FROM players p JOIN users u ON u.id = p.user_id WHERE u.keycloak_subject = $1)`, p.sub); err != nil {
		t.Fatal(err)
	}
	o = p.interact(t, map[string]any{"type": "INSPECT_OBJECT", "object_key": "finance_printer"})
	expectAllowed(t, o)
	if !o.Data.Progress.LevelUp || o.Data.Progress.Level.Code != "ANALYST_I" || len(o.Data.Progress.NewlyAccessibleZones) != 1 || o.Data.Progress.NewlyAccessibleZones[0] != "HQ_FLOOR_3" {
		t.Fatalf("expected level up opening HQ_FLOOR_3: %+v", o.Data.Progress)
	}
	expectAllowed(t, p.interact(t, enter("HQ_FLOOR_3", s, t)))

	// Leaving: only the current zone, and it moves the player to the parent.
	expectDenied(t, p.interact(t, map[string]any{"type": "LEAVE_ZONE", "zone_id": zoneID(t, s.pool, "HQ_FLOOR_2")}), "NOT_IN_ZONE")
	o = p.interact(t, map[string]any{"type": "LEAVE_ZONE", "zone_id": zoneID(t, s.pool, "HQ_FLOOR_3")})
	expectAllowed(t, o)
	if o.Data.Progress.CurrentZone == nil || o.Data.Progress.CurrentZone.Code != "NEXORA_HQ" {
		t.Fatalf("leave zone should land in the parent: %+v", o.Data.Progress)
	}

	// Explicit grants bypass rank for the granted zone only.
	var playerID uuid.UUID
	if err := s.pool.QueryRow(context.Background(),
		`SELECT p.id FROM players p JOIN users u ON u.id = p.user_id WHERE u.keycloak_subject = $1`, p.sub).Scan(&playerID); err != nil {
		t.Fatal(err)
	}
	if err := s.careers.Grant(context.Background(), playerID, zoneID(t, s.pool, "BRANCH"), "instructor"); err != nil {
		t.Fatal(err)
	}
	expectAllowed(t, p.interact(t, enter("BRANCH", s, t)))
	expectDenied(t, p.interact(t, enter("BRANCH_OFFICE", s, t)), "INSUFFICIENT_RANK")

	// Engagement-only actions are refused until engagements exist.
	expectAllowed(t, p.interact(t, enter("HQ_FLOOR_4", s, t)))
	expectDenied(t, p.interact(t, map[string]any{"type": "ACCESS_TERMINAL", "object_key": "redteam_ws_01"}), "REQUIRES_ENGAGEMENT")

	// Progress is persisted (a fresh service sees it).
	fresh := career.NewService(db.New(s.pool))
	st, err := fresh.Load(context.Background(), db.New(s.pool), mustCompany(t, s.pool), playerID, false)
	if err != nil || st.XP != 105 || st.Level.Code != "ANALYST_I" || st.CurrentZoneID == nil || len(st.Unlocks) != 1 {
		t.Fatalf("persisted state %+v %v", st, err)
	}

	// Every attempt was recorded; denials carry their reason.
	var total, denied, missingReason int
	err = s.pool.QueryRow(context.Background(), `SELECT count(*), count(*) FILTER (WHERE outcome = 'denied'),
		count(*) FILTER (WHERE outcome = 'denied' AND reason IS NULL) FROM interactions WHERE player_id = $1`, playerID).Scan(&total, &denied, &missingReason)
	// 17 attempts above, 7 of them denied.
	if err != nil || total != 17 || denied != 7 || missingReason != 0 {
		t.Fatalf("recorded interactions total=%d denied=%d missingReason=%d err=%v", total, denied, missingReason, err)
	}

	// /world reports per-zone access for players.
	var w struct{ Data world.WorldView }
	p.do(t, http.MethodGet, "/api/v1/world", nil, &w)
	for _, z := range w.Data.Zones {
		if z.Access == nil {
			t.Fatalf("zone %s missing access for a player", z.Code)
		}
		if z.Code == "HQ_FLOOR_3" && !z.Access.Accessible {
			t.Fatal("HQ_FLOOR_3 should be accessible after ranking up")
		}
	}
}

func mustCompany(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	c, err := db.New(pool).GetActiveCompany(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return c.ID
}

func TestConcurrentInteractionsCannotDoubleAward(t *testing.T) {
	s := newGameStack(t, nil)
	p := s.player(t)
	expectAllowed(t, p.interact(t, enter("HQ_FLOOR_2", s, t)))

	const n = 20
	var wg sync.WaitGroup
	xp := make([]int32, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			xp[i] = p.interact(t, map[string]any{"type": "USE_WORKSTATION", "object_key": "finance_pc_04"}).Data.XPAwarded
		}()
	}
	wg.Wait()
	var sum int32
	for _, x := range xp {
		sum += x
	}
	if sum != interaction.DiscoveryXP {
		t.Fatalf("discovery XP awarded %d times over, total %d", sum/interaction.DiscoveryXP, sum)
	}
}

func TestInteractionErrors(t *testing.T) {
	s := newGameStack(t, interaction.NewLimiter(1, 3))
	p := s.player(t)

	cases := []struct {
		name string
		body any
		want int
	}{
		{"unknown type", map[string]any{"type": "TELEPORT", "zone_id": uuid.New()}, 400},
		{"missing target", map[string]any{"type": "ENTER_ZONE"}, 400},
		{"unknown field", map[string]any{"type": "ENTER_ZONE", "zone_id": uuid.New(), "xp": 9999}, 400},
	}
	for _, c := range cases {
		if code := p.do(t, http.MethodPost, "/api/v1/interactions", c.body, nil); code != c.want {
			t.Errorf("%s: got %d, want %d", c.name, code, c.want)
		}
	}
	// The burst of 3 is now spent: the next request is rate limited.
	if code := p.do(t, http.MethodPost, "/api/v1/interactions", map[string]any{"type": "ENTER_ZONE", "zone_id": uuid.New()}, nil); code != 429 {
		t.Fatalf("expected 429, got %d", code)
	}

	q := s.player(t)
	if code := q.do(t, http.MethodPost, "/api/v1/interactions", map[string]any{"type": "ENTER_ZONE", "zone_id": uuid.New()}, nil); code != 404 {
		t.Fatalf("unknown zone: %d", code)
	}
	if code := q.do(t, http.MethodPost, "/api/v1/interactions", map[string]any{"type": "USE_WORKSTATION", "object_key": "no_such_object"}, nil); code != 404 {
		t.Fatalf("unknown object: %d", code)
	}

	// Only players interact: an instructor-only account is forbidden.
	staff := &as{s: s, sub: "staff-" + uuid.NewString()[:8], roles: []string{"INSTRUCTOR"}}
	if code := staff.do(t, http.MethodPost, "/api/v1/interactions", map[string]any{"type": "ENTER_ZONE", "zone_id": uuid.New()}, nil); code != 403 {
		t.Fatalf("non-player interaction: %d", code)
	}
}

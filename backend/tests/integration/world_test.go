//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Darkload9999/VORTECH/backend/internal/asset"
	"github.com/Darkload9999/VORTECH/backend/internal/audit"
	"github.com/Darkload9999/VORTECH/backend/internal/auth"
	"github.com/Darkload9999/VORTECH/backend/internal/auth/authtest"
	"github.com/Darkload9999/VORTECH/backend/internal/company"
	"github.com/Darkload9999/VORTECH/backend/internal/database/db"
	"github.com/Darkload9999/VORTECH/backend/internal/employee"
	"github.com/Darkload9999/VORTECH/backend/internal/httpx"
	"github.com/Darkload9999/VORTECH/backend/internal/player"
	"github.com/Darkload9999/VORTECH/backend/internal/scenario"
	"github.com/Darkload9999/VORTECH/backend/internal/world"
)

const nexoraDir = "../../../scenarios/nexora"

func loadNexora(t *testing.T, dir string) *scenario.Bundle {
	t.Helper()
	b, err := scenario.Load(dir)
	if err != nil {
		t.Fatalf("load scenario: %v", err)
	}
	return b
}

// copyScenario copies NEXORA to a temp dir, appending extra to world.yaml.
func copyScenario(t *testing.T, worldSuffix string) string {
	t.Helper()
	dir := t.TempDir()
	entries, err := os.ReadDir(nexoraDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(nexoraDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if e.Name() == "world.yaml" {
			s := strings.Replace(string(b), "\nlocations:\n", worldSuffix+"\nlocations:\n", 1)
			b = []byte(s)
		}
		if err := os.WriteFile(filepath.Join(dir, e.Name()), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func zoneID(t *testing.T, pool *pgxpool.Pool, code string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.QueryRow(context.Background(),
		`SELECT z.id FROM world_zones z JOIN companies c ON c.id = z.company_id
		 JOIN scenarios s ON s.id = c.scenario_id WHERE s.slug = 'nexora' AND z.code = $1`, code).Scan(&id)
	if err != nil {
		t.Fatalf("zone %s: %v", code, err)
	}
	return id
}

func TestScenarioImportLifecycle(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	b := loadNexora(t, nexoraDir)

	res, err := scenario.Import(ctx, pool, b, scenario.ImportOptions{Activate: true})
	if err != nil {
		t.Fatalf("first import: %v", err)
	}
	if res.Outcome != "created" && res.Outcome != "updated" {
		t.Fatalf("outcome = %s", res.Outcome)
	}
	if !res.Activated || res.Counts["employees"] != 31 || res.Counts["departments"] != 8 || res.Counts["zones"] != 9 {
		t.Fatalf("unexpected result %+v", res)
	}
	lobby := zoneID(t, pool, "HQ_LOBBY")

	again, err := scenario.Import(ctx, pool, b, scenario.ImportOptions{})
	if err != nil || again.Outcome != "unchanged" || again.CompanyID != res.CompanyID {
		t.Fatalf("re-import of identical content: %+v %v", again, err)
	}

	// Add a zone, then import the original again: the extra zone is pruned
	// while existing IDs stay stable.
	extra := copyScenario(t, "\n  - { code: TEMP_ANNEX, name: Temporary Annex, kind: area, parent: NEXORA_HQ }")
	withExtra, err := scenario.Import(ctx, pool, loadNexora(t, extra), scenario.ImportOptions{})
	if err != nil || withExtra.Outcome != "updated" || withExtra.Counts["zones"] != 10 {
		t.Fatalf("import with extra zone: %+v %v", withExtra, err)
	}
	pruned, err := scenario.Import(ctx, pool, b, scenario.ImportOptions{})
	if err != nil || pruned.Pruned["zones"] != 1 {
		t.Fatalf("expected the extra zone to be pruned: %+v %v", pruned, err)
	}
	if zoneID(t, pool, "HQ_LOBBY") != lobby {
		t.Fatal("zone IDs must survive re-imports")
	}

	var audits int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action IN ('scenario.imported','scenario.activated') AND resource_id = $1`,
		res.ScenarioID.String()).Scan(&audits); err != nil || audits < 3 {
		t.Fatalf("imports not audited: %d %v", audits, err)
	}

	// Concurrent imports are serialised by an advisory lock and both succeed.
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = scenario.Import(ctx, pool, b, scenario.ImportOptions{Force: true})
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("concurrent import: %v", err)
		}
	}
}

type worldStack struct {
	handler http.Handler
	pool    *pgxpool.Pool
	iss     *authtest.Issuer
}

func newWorldStack(t *testing.T) *worldStack {
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
	ch, eh, ah, wh := company.NewHandler(dir, q, log), employee.NewHandler(dir, q, log), asset.NewHandler(dir, q, log), world.NewHandler(dir, q, log)

	r := httpx.NewRouter()
	p := func(h http.HandlerFunc) http.Handler { return authn.Protect(auth.PermWorldRead, h) }
	r.Handle("GET /api/v1/company", p(ch.Get))
	r.Handle("GET /api/v1/company/departments", p(ch.Departments))
	r.Handle("GET /api/v1/employees", p(eh.List))
	r.Handle("GET /api/v1/employees/{id}", p(eh.Get))
	r.Handle("GET /api/v1/assets", p(ah.List))
	r.Handle("GET /api/v1/assets/graph", p(ah.Graph))
	r.Handle("GET /api/v1/assets/{id}", p(ah.Get))
	r.Handle("GET /api/v1/world", p(wh.World))
	r.Handle("GET /api/v1/world/zones/{id}", p(wh.Zone))
	r.Handle("GET /api/v1/world/objects/{key}", p(wh.Object))
	return &worldStack{handler: httpx.Chain(r, httpx.RequestID, httpx.Recover(log)), pool: pool, iss: iss}
}

func (s *worldStack) get(t *testing.T, target string, out any) int {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, target, nil)
	r.Header.Set("Authorization", "Bearer "+s.iss.Token(t, "player-"+uuid.NewString()[:8], "PLAYER"))
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, r)
	if out != nil && rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("decode %s: %v: %s", target, err, rec.Body.String())
		}
	}
	return rec.Code
}

type envelope[T any] struct {
	Data T              `json:"data"`
	Meta httpx.PageMeta `json:"meta"`
}

func TestWorldAPIs(t *testing.T) {
	s := newWorldStack(t)

	t.Run("company", func(t *testing.T) {
		var c envelope[company.CompanyView]
		if code := s.get(t, "/api/v1/company", &c); code != 200 {
			t.Fatalf("status %d", code)
		}
		if c.Data.Code != "NEXORA" || c.Data.EmailDomain != "nexora.local" || c.Data.Stats.Employees != 31 || c.Data.Scenario.Slug != "nexora" {
			t.Fatalf("unexpected company %+v", c.Data)
		}
		var d envelope[[]company.DepartmentView]
		s.get(t, "/api/v1/company/departments", &d)
		if len(d.Data) != 8 || d.Data[0].Code != "EXEC" || d.Data[0].Head == nil {
			t.Fatalf("unexpected departments %+v", d.Data)
		}
	})

	t.Run("employee pagination and filters", func(t *testing.T) {
		var seen []string
		cursor := ""
		for page := 0; page < 10; page++ {
			var e envelope[[]employee.Summary]
			target := "/api/v1/employees?limit=10"
			if cursor != "" {
				target += "&cursor=" + url.QueryEscape(cursor)
			}
			if code := s.get(t, target, &e); code != 200 {
				t.Fatalf("status %d", code)
			}
			for _, x := range e.Data {
				seen = append(seen, x.Code)
			}
			if e.Meta.NextCursor == nil {
				break
			}
			cursor = *e.Meta.NextCursor
		}
		if len(seen) != 31 || seen[0] != "EMP-0001" || seen[30] != "EMP-0031" {
			t.Fatalf("paginated %d employees: %v", len(seen), seen)
		}

		var fin envelope[[]employee.Summary]
		s.get(t, "/api/v1/employees?department=FIN", &fin)
		if len(fin.Data) != 6 {
			t.Fatalf("FIN employees = %d", len(fin.Data))
		}
		var search envelope[[]employee.Summary]
		s.get(t, "/api/v1/employees?q=accountant", &search)
		if len(search.Data) != 2 {
			t.Fatalf("search 'accountant' = %d", len(search.Data))
		}
		// LIKE wildcards are matched literally, not as patterns.
		var wild envelope[[]employee.Summary]
		s.get(t, "/api/v1/employees?q=%25", &wild)
		if len(wild.Data) != 0 {
			t.Fatalf("'%%' must not match everything, got %d", len(wild.Data))
		}
		if code := s.get(t, "/api/v1/employees?department=fin%27--", nil); code != 400 {
			t.Fatalf("malformed department filter: %d", code)
		}
	})

	t.Run("employee detail matches the spec example", func(t *testing.T) {
		var list envelope[[]employee.Summary]
		s.get(t, "/api/v1/employees?q=EMP-0018", &list)
		if len(list.Data) != 1 {
			t.Fatalf("lookup EMP-0018: %+v", list.Data)
		}
		var d envelope[employee.Detail]
		if code := s.get(t, "/api/v1/employees/"+list.Data[0].ID.String(), &d); code != 200 {
			t.Fatalf("status %d", code)
		}
		e := d.Data
		if e.DisplayName != "Alex Morgan" || e.Department.Code != "FIN" || e.Manager == nil || e.Manager.Code != "EMP-0008" {
			t.Fatalf("unexpected employee %+v", e)
		}
		if len(e.Identities) != 1 || e.Identities[0].Email != "alex@nexora.local" || len(e.Identities[0].Groups) != 2 {
			t.Fatalf("unexpected identities %+v", e.Identities)
		}
		if len(e.Assets) != 1 || e.Assets[0].Code != "HQ-FIN-PC-04" {
			t.Fatalf("unexpected assets %+v", e.Assets)
		}
		if len(e.Schedule) != 15 || e.Schedule[0].From != "09:00" || e.Schedule[0].Weekday != 1 {
			t.Fatalf("unexpected schedule %+v", e.Schedule)
		}
		if code := s.get(t, "/api/v1/employees/"+uuid.NewString(), nil); code != 404 {
			t.Fatalf("unknown employee: %d", code)
		}
		if code := s.get(t, "/api/v1/employees/not-a-uuid", nil); code != 404 {
			t.Fatalf("malformed id: %d", code)
		}
	})

	t.Run("persona is not exposed", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/world/zones/"+zoneID(t, s.pool, "HQ_FLOOR_2").String(), nil)
		r.Header.Set("Authorization", "Bearer "+s.iss.Token(t, "p", "PLAYER"))
		rec := httptest.NewRecorder()
		s.handler.ServeHTTP(rec, r)
		if strings.Contains(rec.Body.String(), "reuses the same password") || strings.Contains(rec.Body.String(), "persona") {
			t.Fatal("NPC persona (an authoring note) leaked to players")
		}
	})

	t.Run("assets", func(t *testing.T) {
		var servers envelope[[]asset.Summary]
		s.get(t, "/api/v1/assets?type=server", &servers)
		if len(servers.Data) != 9 { // DC01 WEB01 APP01 DB01 GIT01 JUMP01 MAIL01 FS01 SIEM01
			t.Fatalf("servers = %d", len(servers.Data))
		}
		var nets envelope[[]asset.Summary]
		s.get(t, "/api/v1/assets?type=network&limit=200", &nets)
		if len(nets.Data) != 10 || nets.Data[0].NetworkCIDR == nil {
			t.Fatalf("networks = %+v", nets.Data)
		}
		if code := s.get(t, "/api/v1/assets?type=spaceship", nil); code != 400 {
			t.Fatalf("invalid type: %d", code)
		}
		var pc envelope[[]asset.Summary]
		s.get(t, "/api/v1/assets?q=FIN-PC04", &pc)
		if len(pc.Data) != 1 {
			t.Fatalf("hostname search: %+v", pc.Data)
		}
		var d envelope[asset.Detail]
		s.get(t, "/api/v1/assets/"+pc.Data[0].ID.String(), &d)
		if d.Data.Owner == nil || d.Data.Owner.Code != "EMP-0018" || len(d.Data.Networks) != 1 ||
			d.Data.Networks[0].Code != "NET-HQ-FINANCE" || len(d.Data.WorldObjects) != 1 || d.Data.WorldObjects[0].Key != "finance_pc_04" {
			t.Fatalf("unexpected asset detail %+v", d.Data)
		}
	})

	t.Run("digital twin graph", func(t *testing.T) {
		var full envelope[asset.Graph]
		s.get(t, "/api/v1/assets/graph", &full)
		if len(full.Data.Nodes) != 31+36+78 || full.Data.Truncated {
			t.Fatalf("full graph nodes = %d", len(full.Data.Nodes))
		}

		var list envelope[[]employee.Summary]
		s.get(t, "/api/v1/employees?q=EMP-0018", &list)
		root := "employee:" + list.Data[0].ID.String()
		var g envelope[asset.Graph]
		if code := s.get(t, "/api/v1/assets/graph?depth=2&root="+root, &g); code != 200 {
			t.Fatalf("status %d", code)
		}
		codes := map[string]bool{}
		for _, n := range g.Data.Nodes {
			codes[n.Data.Code] = true
		}
		for _, want := range []string{"EMP-0018", "alex", "HQ-FIN-PC-04", "GRP-FINANCE", "APP-LEDGER", "NET-HQ-FINANCE", "EMP-0008"} {
			if !codes[want] {
				t.Errorf("graph around Alex is missing %s", want)
			}
		}
		for _, e := range g.Data.Edges {
			if !codes[nodeCode(g.Data, e.Data.Source)] || !codes[nodeCode(g.Data, e.Data.Target)] {
				t.Fatalf("edge %s references a node outside the subgraph", e.Data.ID)
			}
		}
		if code := s.get(t, "/api/v1/assets/graph?root=asset:"+uuid.NewString(), nil); code != 404 {
			t.Fatalf("unknown root: %d", code)
		}
		if code := s.get(t, "/api/v1/assets/graph?depth=9", nil); code != 400 {
			t.Fatalf("excessive depth: %d", code)
		}
	})

	t.Run("world and zones", func(t *testing.T) {
		var w envelope[world.WorldView]
		s.get(t, "/api/v1/world", &w)
		if len(w.Data.Zones) != 9 || w.Data.Company.Code != "NEXORA" {
			t.Fatalf("unexpected world %+v", w.Data)
		}
		var z envelope[world.ZoneDetail]
		s.get(t, "/api/v1/world/zones/"+zoneID(t, s.pool, "HQ_FLOOR_4").String(), &z)
		if z.Data.Code != "HQ_FLOOR_4" || len(z.Data.Locations) != 3 || len(z.Data.NPCs) != 8 { // security 2, SOC 4, red team 2
			t.Fatalf("floor 4: %d locations, %d npcs", len(z.Data.Locations), len(z.Data.NPCs))
		}
		var rt *world.Object
		for i := range z.Data.Objects {
			if z.Data.Objects[i].Key == "redteam_ws_01" {
				rt = &z.Data.Objects[i]
			}
		}
		if rt == nil || rt.Asset == nil || rt.Asset.Code != "HQ-RT-PC-03" || len(rt.Interactions) != 4 {
			t.Fatalf("red team workstation: %+v", rt)
		}
	})

	t.Run("three.js object resolves to the digital asset", func(t *testing.T) {
		var o envelope[world.ObjectDetail]
		if code := s.get(t, "/api/v1/world/objects/finance_pc_04", &o); code != 200 {
			t.Fatalf("status %d", code)
		}
		a := o.Data.Asset
		if a == nil || a.Code != "HQ-FIN-PC-04" || *a.Hostname != "FIN-PC04" || a.IPAddress.String() != "10.20.30.44" ||
			a.Owner == nil || a.Owner.Code != "EMP-0018" || *a.DepartmentCode != "FIN" ||
			len(a.Networks) != 1 || a.Networks[0].Code != "NET-HQ-FINANCE" || o.Data.Zone.Code != "HQ_FLOOR_2" {
			t.Fatalf("finance_pc_04 does not resolve as specified: %+v / %+v", o.Data, a)
		}
		var door envelope[world.ObjectDetail]
		s.get(t, "/api/v1/world/objects/lobby_elevator_f4", &door)
		if door.Data.LeadsTo == nil || door.Data.LeadsTo.Code != "HQ_FLOOR_4" || door.Data.Asset != nil {
			t.Fatalf("door: %+v", door.Data)
		}
		for _, key := range []string{"does_not_exist", "BAD%20KEY"} {
			if code := s.get(t, "/api/v1/world/objects/"+key, nil); code != 404 {
				t.Fatalf("%s: %d", key, code)
			}
		}
	})

	t.Run("no active world", func(t *testing.T) {
		if _, err := s.pool.Exec(context.Background(), "UPDATE scenarios SET is_active = false"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = s.pool.Exec(context.Background(), "UPDATE scenarios SET is_active = true WHERE slug = 'nexora'")
		})
		if code := s.get(t, "/api/v1/company", nil); code != 503 {
			t.Fatalf("expected 503 WORLD_NOT_ACTIVE, got %d", code)
		}
	})
}

func nodeCode(g asset.Graph, id asset.NodeID) string {
	for _, n := range g.Nodes {
		if n.Data.ID == id {
			return n.Data.Code
		}
	}
	return ""
}

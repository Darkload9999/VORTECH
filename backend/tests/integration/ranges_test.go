//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Darkload9999/VORTECH/backend/internal/audit"
	"github.com/Darkload9999/VORTECH/backend/internal/auth"
	"github.com/Darkload9999/VORTECH/backend/internal/auth/authtest"
	"github.com/Darkload9999/VORTECH/backend/internal/cyberrange"
	"github.com/Darkload9999/VORTECH/backend/internal/cyberrange/controller"
	"github.com/Darkload9999/VORTECH/backend/internal/cyberrange/kube"
	"github.com/Darkload9999/VORTECH/backend/internal/database/db"
	"github.com/Darkload9999/VORTECH/backend/internal/event"
	"github.com/Darkload9999/VORTECH/backend/internal/httpx"
	"github.com/Darkload9999/VORTECH/backend/internal/jobs"
	"github.com/Darkload9999/VORTECH/backend/internal/player"
	"github.com/Darkload9999/VORTECH/backend/internal/scenario"
)

// fakeCluster is an in-memory kube.Cluster. The real client is covered by
// kube's unit tests and by TestRangeOnRealCluster.
type fakeCluster struct {
	mu       sync.Mutex
	ns       map[string]kube.ManagedNamespace
	readyErr error
	applyErr error
	applied  int
}

func newFakeCluster() *fakeCluster { return &fakeCluster{ns: map[string]kube.ManagedNamespace{}} }

func (f *fakeCluster) Apply(_ context.Context, b kube.Bundle) ([]kube.ResourceRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.applyErr != nil {
		return nil, f.applyErr
	}
	f.applied++
	f.ns[b.Namespace.Name] = kube.ManagedNamespace{Name: b.Namespace.Name, RangeID: b.Namespace.Labels[kube.LabelRangeID]}
	refs := []kube.ResourceRef{{Kind: "Namespace", Name: b.Namespace.Name}}
	for _, d := range b.Deployments {
		refs = append(refs, kube.ResourceRef{Kind: "Deployment", Name: d.Name})
	}
	return refs, nil
}

func (f *fakeCluster) WaitReady(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.readyErr
}

func (f *fakeCluster) Delete(_ context.Context, ns string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.ns, ns)
	return nil
}

func (f *fakeCluster) WaitGone(context.Context, string) error { return nil }

func (f *fakeCluster) Exists(_ context.Context, ns string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.ns[ns]
	return ok, nil
}

func (f *fakeCluster) ListManaged(context.Context) ([]kube.ManagedNamespace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]kube.ManagedNamespace, 0, len(f.ns))
	for _, n := range f.ns {
		out = append(out, n)
	}
	return out, nil
}

func (f *fakeCluster) Quarantine(_ context.Context, ns string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.ns[ns]
	n.Quarantined = true
	f.ns[ns] = n
	return nil
}

func (f *fakeCluster) set(fn func(f *fakeCluster)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeCluster) has(ns string) bool { ok, _ := f.Exists(context.Background(), ns); return ok }

type rangeStack struct {
	handler http.Handler
	pool    *pgxpool.Pool
	iss     *authtest.Issuer
	cluster *fakeCluster
	ctrl    *controller.Controller
	runner  *jobs.Runner

	mu     sync.Mutex
	events []event.Event
}

func ctrlConfig(cpuMillis int64) controller.Config {
	return controller.Config{
		// Room for exactly one nexora-corp-net (950m) at a time.
		Capacity: cyberrange.Capacity{
			Cluster: cyberrange.Resources{CPUMillis: cpuMillis, MemoryMiB: 1 << 20, StorageMiB: 1 << 20},
		},
		ImageAllowlist:   []string{"docker.io/library/", "docker.io/nginxinc/", "docker.io/traefik/", "docker.io/gitea/"},
		OrphanPolicy:     controller.OrphanDelete,
		ProvisionTimeout: 5 * time.Second,
		DestroyTimeout:   5 * time.Second,
		StaleAfter:       time.Hour,
	}
}

func newRangeStack(t *testing.T) *rangeStack {
	t.Helper()
	pool := migratedPool(t)
	if _, err := scenario.Import(context.Background(), pool, loadNexora(t, nexoraDir), scenario.ImportOptions{Activate: true}); err != nil {
		t.Fatal(err)
	}
	iss, v := fakeIssuerVerifier(t)
	q := db.New(pool)
	log := testLogger()
	authn := auth.NewAuthenticator(v, player.NewDirectory(pool, log, 0), audit.NewRecorder(q, log), log)
	h := cyberrange.NewHandler(cyberrange.NewService(pool), log)

	r := httpx.NewRouter()
	r.Handle("GET /api/v1/range-templates", authn.Protect(auth.PermRangeUseOwn, http.HandlerFunc(h.Templates)))
	r.Handle("POST /api/v1/ranges", authn.Protect(auth.PermRangeUseOwn, http.HandlerFunc(h.Create)))
	r.Handle("GET /api/v1/ranges", authn.Protect(auth.PermRangeUseOwn, http.HandlerFunc(h.List)))
	r.Handle("GET /api/v1/ranges/{id}", authn.Protect(auth.PermRangeUseOwn, http.HandlerFunc(h.Get)))
	r.Handle("DELETE /api/v1/ranges/{id}", authn.Protect(auth.PermRangeUseOwn, http.HandlerFunc(h.Destroy)))

	s := &rangeStack{
		handler: httpx.Chain(r, httpx.RequestID, httpx.Recover(log)),
		pool:    pool, iss: iss, cluster: newFakeCluster(),
	}
	s.ctrl = controller.New(pool, s.cluster, log, ctrlConfig(1000))
	s.runner = jobs.NewRunner(pool, log, jobs.Config{Poll: 50 * time.Millisecond, BackoffBase: 10 * time.Millisecond})
	s.ctrl.Register(s.runner)

	// Events reach subscribers only through PostgreSQL, as in production.
	bus := event.NewBus(log)
	bus.Subscribe("test", nil, func(_ context.Context, e event.Event) {
		s.mu.Lock()
		s.events = append(s.events, e)
		s.mu.Unlock()
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); event.Bridge(ctx, pool, log, bus) }()
	t.Cleanup(func() {
		cancel()
		<-done
		_ = bus.Close(context.Background())
	})
	time.Sleep(100 * time.Millisecond) // let the listener subscribe
	return s
}

func (s *rangeStack) call(t *testing.T, sub string, roles []string, method, target string, body any) (int, map[string]any) {
	t.Helper()
	var rd bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = *bytes.NewReader(b)
	}
	r := httptest.NewRequest(method, target, &rd)
	r.Header.Set("Authorization", "Bearer "+s.iss.Token(t, sub, roles...))
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, r)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func (s *rangeStack) create(t *testing.T, sub, template string) uuid.UUID {
	t.Helper()
	code, out := s.call(t, sub, []string{"PLAYER"}, http.MethodPost, "/api/v1/ranges", map[string]string{"template": template})
	if code != http.StatusAccepted {
		t.Fatalf("create %s: %d %v", template, code, out)
	}
	data := out["data"].(map[string]any)
	if data["state"] != "REQUESTED" {
		t.Fatalf("new range state %v", data["state"])
	}
	return uuid.MustParse(data["id"].(string))
}

func (s *rangeStack) state(t *testing.T, id uuid.UUID) db.Range {
	t.Helper()
	row, err := db.New(s.pool).GetRangeView(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return row.Range
}

func (s *rangeStack) waitState(t *testing.T, id uuid.UUID, want cyberrange.State) db.Range {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		r := s.state(t, id)
		if cyberrange.State(r.State) == want {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("range %s: state %s, want %s (reason %v)", id, r.State, want, deref(r.FailureReason))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (s *rangeStack) eventTypes(playerTopic string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, e := range s.events {
		if e.Topic == playerTopic {
			out = append(out, e.Type)
		}
	}
	return out
}

func (s *rangeStack) startRunner(t *testing.T) {
	t.Helper()
	startRunner(t, s.runner)
}

func playerIDOf(t *testing.T, pool *pgxpool.Pool, rangeID uuid.UUID) uuid.UUID {
	t.Helper()
	row, err := db.New(pool).GetRangeView(context.Background(), rangeID)
	if err != nil {
		t.Fatal(err)
	}
	return row.Range.PlayerID
}

// TestRangeLifecycle drives the whole flow through the API, the job queue
// and the controller: admission under capacity, FIFO queueing, readiness,
// isolation between players, destroy, expiry, failures and events.
func TestRangeLifecycle(t *testing.T) {
	s := newRangeStack(t)
	alice, bob := "alice-"+uuid.NewString()[:8], "bob-"+uuid.NewString()[:8]
	player := []string{"PLAYER"}

	code, out := s.call(t, alice, player, http.MethodGet, "/api/v1/range-templates", nil)
	if code != 200 || len(out["data"].([]any)) != 2 {
		t.Fatalf("templates: %d %v", code, out)
	}
	if code, out := s.call(t, alice, player, http.MethodPost, "/api/v1/ranges", map[string]string{"template": "nope-template"}); code != 422 {
		t.Fatalf("unknown template: %d %v", code, out)
	}

	a := s.create(t, alice, "nexora-corp-net")
	if code, out := s.call(t, alice, player, http.MethodPost, "/api/v1/ranges", map[string]string{"template": "nexora-web-basics"}); code != 409 ||
		out["error"].(map[string]any)["code"] != cyberrange.CodeRangeLimit {
		t.Fatalf("second live range: %d %v", code, out)
	}
	b := s.create(t, bob, "nexora-web-basics")

	// Only one range fits: Alice's is admitted, Bob's waits.
	s.startRunner(t)
	ra := s.waitState(t, a, cyberrange.Ready)
	rb := s.waitState(t, b, cyberrange.Queued)
	if ra.ExpiresAt == nil || ra.ReadyAt == nil || ra.ExpiresAt.Sub(*ra.ReadyAt) != 2*time.Hour {
		t.Fatalf("expiry not set from the template TTL: %+v", ra)
	}
	if !s.cluster.has(ra.Namespace) {
		t.Fatal("namespace not applied")
	}
	if n := countRows(t, s.pool, "SELECT count(*) FROM range_resources WHERE range_id = $1", a); n != 6 {
		t.Errorf("recorded resources = %d, want namespace + 5 deployments", n)
	}

	// Isolation: Bob cannot see Alice's range; a playing administrator can.
	if code, _ := s.call(t, bob, player, http.MethodGet, "/api/v1/ranges/"+a.String(), nil); code != 404 {
		t.Errorf("other player's range: %d", code)
	}
	if code, _ := s.call(t, bob, player, http.MethodDelete, "/api/v1/ranges/"+a.String(), nil); code != 404 {
		t.Errorf("deleting other player's range: %d", code)
	}
	if code, _ := s.call(t, "admin-"+uuid.NewString()[:8], []string{"PLAYER", "ADMIN"}, http.MethodGet, "/api/v1/ranges/"+a.String(), nil); code != 200 {
		t.Errorf("admin view: %d", code)
	}
	code, out = s.call(t, alice, player, http.MethodGet, "/api/v1/ranges", nil)
	if code != 200 || len(out["data"].([]any)) != 1 {
		t.Fatalf("list: %d %v", code, out)
	}

	// Destroying Alice's range frees capacity and admits Bob's.
	code, out = s.call(t, alice, player, http.MethodDelete, "/api/v1/ranges/"+a.String(), nil)
	if code != 202 || out["data"].(map[string]any)["state"] != "STOPPING" {
		t.Fatalf("destroy: %d %v", code, out)
	}
	if code, out := s.call(t, alice, player, http.MethodDelete, "/api/v1/ranges/"+a.String(), nil); code != 202 {
		t.Fatalf("destroy must be idempotent: %d %v", code, out)
	}
	s.waitState(t, a, cyberrange.Destroyed)
	if s.cluster.has(ra.Namespace) {
		t.Fatal("namespace not deleted")
	}
	rb = s.waitState(t, b, cyberrange.Ready)

	// Expiry: Bob's range runs out of time.
	if _, err := s.pool.Exec(context.Background(), "UPDATE ranges SET expires_at = now() - interval '1 second' WHERE id = $1", b); err != nil {
		t.Fatal(err)
	}
	if n, err := s.ctrl.ExpireDue(context.Background()); err != nil || n != 1 {
		t.Fatalf("expire: %d %v", n, err)
	}
	s.waitState(t, b, cyberrange.Destroyed)
	if s.cluster.has(rb.Namespace) {
		t.Fatal("expired range not cleaned up")
	}

	// A workload that cannot start fails the range permanently and it is
	// cleaned up; the player sees a safe reason, operators the detail.
	s.cluster.set(func(f *fakeCluster) {
		f.readyErr = fmt.Errorf("%w: db01-x/db01: CreateContainerConfigError: secret missing", kube.ErrPermanent)
	})
	c := s.create(t, alice, "nexora-web-basics")
	s.waitState(t, c, cyberrange.Destroyed)
	s.cluster.set(func(f *fakeCluster) { f.readyErr = nil })
	rc := s.state(t, c)
	if deref(rc.FailureReason) != "a range workload could not start" {
		t.Fatalf("failure reason %q", deref(rc.FailureReason))
	}
	var lastErr string
	if err := s.pool.QueryRow(context.Background(),
		"SELECT last_error FROM jobs WHERE kind = 'range.provision' AND payload->>'range_id' = $1", c.String()).Scan(&lastErr); err != nil || lastErr == "" {
		t.Fatalf("provision job error not recorded: %q %v", lastErr, err)
	}

	// History, events and notifications.
	hist, err := db.New(s.pool).ListRangeTransitions(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	var path []string
	for _, h := range hist {
		path = append(path, h.ToState)
	}
	want := []string{"REQUESTED", "QUEUED", "PROVISIONING", "STARTING", "READY", "EXPIRED", "STOPPING", "DESTROYED"}
	if fmt.Sprint(path) != fmt.Sprint(want) {
		t.Fatalf("bob's history %v, want %v", path, want)
	}
	bobTopic := event.PlayerTopic(playerIDOf(t, s.pool, b))
	deadline := time.Now().Add(5 * time.Second)
	for {
		types := s.eventTypes(bobTopic)
		if len(types) >= len(want)+2 { // + notification.created for READY and EXPIRED
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("events for bob: %v", types)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := countRows(t, s.pool, `SELECT count(*) FROM notifications n JOIN players p ON p.user_id = n.user_id
		JOIN ranges r ON r.player_id = p.id WHERE r.id = $1 AND n.type IN ('range.ready', 'range.expired')`, b); n != 2 {
		t.Errorf("bob's notifications = %d, want ready + expired", n)
	}
	if n := countRows(t, s.pool, "SELECT count(*) FROM audit_logs WHERE resource_id = $1 AND action IN ('range.created', 'range.provisioned', 'range.destroyed')", b.String()); n != 3 {
		t.Errorf("audit entries for bob's range = %d", n)
	}
	if n := countRows(t, s.pool, "SELECT count(*) FROM ranges WHERE state <> 'DESTROYED'"); n != 0 {
		t.Fatalf("%d ranges left behind", n)
	}
}

func TestRangeAdmissionRejectsOversizedRanges(t *testing.T) {
	s := newRangeStack(t)
	ctrl := controller.New(s.pool, s.cluster, testLogger(), ctrlConfig(500)) // corp-net needs 950m
	id := s.create(t, "big-"+uuid.NewString()[:8], "nexora-corp-net")
	if _, err := ctrl.Admit(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := s.state(t, id)
	if r.State != "FAILED" || deref(r.FailureReason) != "the range exceeds the platform's total cpu capacity" {
		t.Fatalf("oversized range: %s %q", r.State, deref(r.FailureReason))
	}
	s.startRunner(t)
	s.waitState(t, id, cyberrange.Destroyed)
}

func TestRangeReconciliation(t *testing.T) {
	s := newRangeStack(t)
	ctx := context.Background()
	s.startRunner(t)

	// A live range whose namespace vanished is failed and cleaned up.
	id := s.create(t, "rec-"+uuid.NewString()[:8], "nexora-web-basics")
	r := s.waitState(t, id, cyberrange.Ready)
	_ = s.cluster.Delete(ctx, r.Namespace)

	// A managed namespace nobody owns is an orphan.
	orphan := cyberrange.NamespaceFor(uuid.New())
	s.cluster.set(func(f *fakeCluster) { f.ns[orphan] = kube.ManagedNamespace{Name: orphan, RangeID: "x"} })

	rep, err := s.ctrl.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Orphans != 1 || rep.Vanished != 1 {
		t.Fatalf("report %+v", rep)
	}
	if s.cluster.has(orphan) {
		t.Fatal("orphan not deleted")
	}
	s.waitState(t, id, cyberrange.Destroyed)
	if got := deref(s.state(t, id).FailureReason); got != "the range's environment disappeared" {
		t.Fatalf("reason %q", got)
	}

	// Quarantine policy keeps the namespace for inspection.
	cfg := ctrlConfig(1000)
	cfg.OrphanPolicy = controller.OrphanQuarantine
	q := controller.New(s.pool, s.cluster, testLogger(), cfg)
	orphan2 := cyberrange.NamespaceFor(uuid.New())
	s.cluster.set(func(f *fakeCluster) { f.ns[orphan2] = kube.ManagedNamespace{Name: orphan2} })
	if rep, err := q.Reconcile(ctx); err != nil || rep.Orphans != 1 || !s.cluster.has(orphan2) {
		t.Fatalf("quarantine: %+v %v", rep, err)
	}
	if rep, _ := q.Reconcile(ctx); rep.Orphans != 0 {
		t.Fatal("a quarantined namespace must be left alone")
	}
	_ = s.cluster.Delete(ctx, orphan2)
}

func TestRangeStuckTeardownIsRedriven(t *testing.T) {
	s := newRangeStack(t)
	ctx := context.Background()
	sub := "stuck-" + uuid.NewString()[:8]
	id := s.create(t, sub, "nexora-web-basics")

	run := func(c *controller.Controller) context.CancelFunc {
		r := jobs.NewRunner(s.pool, testLogger(), jobs.Config{Poll: 20 * time.Millisecond, BackoffBase: time.Millisecond, BackoffMax: 2 * time.Millisecond})
		c.Register(r)
		rctx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { defer close(done); _ = r.Run(rctx) }()
		return func() { cancel(); <-done }
	}

	stop := run(s.ctrl)
	ready := s.waitState(t, id, cyberrange.Ready)
	stop()

	// Teardown keeps failing until its job gives up: the range is stuck in
	// STOPPING with no job left to finish it.
	stop = run(controller.New(s.pool, deleteFails{s.cluster}, testLogger(), ctrlConfig(1000)))
	if code, out := s.call(t, sub, []string{"PLAYER"}, http.MethodDelete, "/api/v1/ranges/"+id.String(), nil); code != 202 {
		t.Fatalf("destroy: %d %v", code, out)
	}
	deadline := time.Now().Add(15 * time.Second)
	for countRows(t, s.pool, "SELECT count(*) FROM jobs WHERE kind = 'range.destroy' AND state = 'failed' AND payload->>'range_id' = $1", id.String()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("destroy job never gave up")
		}
		time.Sleep(20 * time.Millisecond)
	}
	stop()
	if st := s.state(t, id).State; st != "STOPPING" || !s.cluster.has(ready.Namespace) {
		t.Fatalf("state %s", st)
	}

	// The reconciler notices and re-drives the teardown.
	cfg := ctrlConfig(1000)
	cfg.StaleAfter = time.Millisecond
	rep, err := controller.New(s.pool, s.cluster, testLogger(), cfg).Reconcile(ctx)
	if err != nil || rep.Redriven != 1 {
		t.Fatalf("reconcile: %+v %v", rep, err)
	}
	stop = run(s.ctrl)
	defer stop()
	s.waitState(t, id, cyberrange.Destroyed)
	if s.cluster.has(ready.Namespace) {
		t.Fatal("namespace survived")
	}
}

// deleteFails is a cluster whose API server rejects deletes.
type deleteFails struct{ *fakeCluster }

func (deleteFails) Delete(context.Context, string) error { return errors.New("api server unavailable") }

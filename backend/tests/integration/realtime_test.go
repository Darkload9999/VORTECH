//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ws "github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Darkload9999/VORTECH/backend/internal/audit"
	"github.com/Darkload9999/VORTECH/backend/internal/auth"
	"github.com/Darkload9999/VORTECH/backend/internal/auth/authtest"
	"github.com/Darkload9999/VORTECH/backend/internal/career"
	"github.com/Darkload9999/VORTECH/backend/internal/company"
	"github.com/Darkload9999/VORTECH/backend/internal/config"
	"github.com/Darkload9999/VORTECH/backend/internal/database/db"
	"github.com/Darkload9999/VORTECH/backend/internal/event"
	"github.com/Darkload9999/VORTECH/backend/internal/httpx"
	"github.com/Darkload9999/VORTECH/backend/internal/interaction"
	"github.com/Darkload9999/VORTECH/backend/internal/notification"
	"github.com/Darkload9999/VORTECH/backend/internal/player"
	"github.com/Darkload9999/VORTECH/backend/internal/scenario"
	"github.com/Darkload9999/VORTECH/backend/internal/websocket"
	"github.com/Darkload9999/VORTECH/backend/internal/world"
)

// rtStack is the API's real-time wiring on a real HTTP server.
type rtStack struct {
	srv  *httptest.Server
	pool *pgxpool.Pool
	iss  *authtest.Issuer
	gw   *websocket.Gateway
}

func newRealtimeStack(t *testing.T) *rtStack {
	t.Helper()
	pool := migratedPool(t)
	if _, err := scenario.Import(context.Background(), pool, loadNexora(t, nexoraDir), scenario.ImportOptions{Activate: true}); err != nil {
		t.Fatal(err)
	}
	iss, v := fakeIssuerVerifier(t)
	q := db.New(pool)
	log := testLogger()
	rec := audit.NewRecorder(q, log)
	authn := auth.NewAuthenticator(v, player.NewDirectory(pool, log, 0), rec, log)
	dir := company.NewDirectory(q, 0)
	careers := career.NewService(q)

	bus := event.NewBus(log)
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	cfg := config.RealtimeConfig{MaxConnections: 100, MaxConnectionsPerUser: 5, MaxConnectionAge: time.Hour, PingInterval: time.Minute, TicketTTL: time.Minute}
	gw := websocket.NewGateway(cfg, websocket.NewTicketStore(cfg.TicketTTL),
		websocket.WorldAuthorizer{Dir: dir, Q: q, Access: careers}, bus, log)
	bus.Subscribe("gateway", func(e event.Event) bool { return e.Topic != "" }, gw.Deliver)

	r := httpx.NewRouter()
	protect := func(p auth.Permission, h http.HandlerFunc) http.Handler { return authn.Protect(p, h) }
	r.Handle("POST /api/v1/interactions", protect(auth.PermScenarioPlay,
		interaction.NewHandler(interaction.NewService(pool, dir, careers, bus), nil, log).Create))
	r.Handle("POST /api/v1/ws/tickets", protect(auth.PermProfileReadOwn, gw.IssueTicket))
	nh := notification.NewHandler(q, log)
	r.Handle("GET /api/v1/notifications", protect(auth.PermProfileReadOwn, nh.List))
	r.Handle("POST /api/v1/notifications/{id}/read", protect(auth.PermProfileReadOwn, nh.MarkRead))
	r.Handle("POST /api/v1/notifications/read-all", protect(auth.PermProfileReadOwn, nh.MarkAllRead))
	r.Handle("POST /api/v1/admin/announcements", protect(auth.PermPlatformAdminister, world.NewAnnouncer(dir, bus, rec, log).Create))

	mux := http.NewServeMux()
	mux.Handle("GET /ws", http.HandlerFunc(gw.Serve))
	mux.Handle("/", httpx.Chain(r, httpx.RequestID, httpx.Recover(log)))
	srv := httptest.NewUnstartedServer(mux)
	// Short server timeouts prove the WebSocket survives them.
	srv.Config.ReadTimeout, srv.Config.WriteTimeout = 2*time.Second, 2*time.Second
	srv.Start()
	t.Cleanup(srv.Close)
	return &rtStack{srv: srv, pool: pool, iss: iss, gw: gw}
}

type rtUser struct {
	s     *rtStack
	sub   string
	roles []string
}

func (s *rtStack) user(roles ...string) *rtUser {
	return &rtUser{s: s, sub: "rt-" + uuid.NewString()[:8], roles: roles}
}

func (u *rtUser) call(t *testing.T, method, path string, body, out any) int {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, u.s.srv.URL+path, rd)
	req.Header.Set("Authorization", "Bearer "+u.s.iss.Token(t, u.sub, u.roles...))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode < 300 {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode
}

// connect opens a WebSocket via the ticket flow and returns the welcome.
func (u *rtUser) connect(t *testing.T) (*ws.Conn, map[string]any) {
	t.Helper()
	var tk struct {
		Data struct {
			Ticket string `json:"ticket"`
		} `json:"data"`
	}
	if code := u.call(t, http.MethodPost, "/api/v1/ws/tickets", nil, &tk); code != 201 {
		t.Fatalf("ticket status %d", code)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := ws.Dial(ctx, "ws"+strings.TrimPrefix(u.s.srv.URL, "http")+"/ws?ticket="+tk.Data.Ticket, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	w := frame(t, c)
	if w["type"] != "session.welcome" {
		t.Fatalf("first frame %v", w)
	}
	return c, w
}

func frame(t *testing.T, c *ws.Conn) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, b, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

// frames reads until n frames arrived and returns their types in order.
func frames(t *testing.T, c *ws.Conn, n int) ([]string, []map[string]any) {
	t.Helper()
	var types []string
	var all []map[string]any
	for range n {
		m := frame(t, c)
		types = append(types, m["type"].(string))
		all = append(all, m)
	}
	return types, all
}

func TestRealtimeEndToEnd(t *testing.T) {
	s := newRealtimeStack(t)
	alice, bob := s.user("PLAYER"), s.user("PLAYER")
	ca, welcome := alice.connect(t)
	cb, _ := bob.connect(t)

	topics := welcome["data"].(map[string]any)["topics"].([]any)
	if len(topics) != 2 || !strings.HasPrefix(topics[0].(string), "player:") || !strings.HasPrefix(topics[1].(string), "world:") {
		t.Fatalf("welcome topics %v", topics)
	}

	// Outlive the server's 2s read/write timeouts before acting.
	time.Sleep(2500 * time.Millisecond)

	// Alice acts over REST; the events arrive on her socket after commit.
	if code := alice.call(t, http.MethodPost, "/api/v1/interactions", enterBody(t, s.pool, "HQ_FLOOR_2"), nil); code != 200 {
		t.Fatalf("enter: %d", code)
	}
	types, _ := frames(t, ca, 2)
	if strings.Join(types, ",") != "player.entered_zone,player.interacted" {
		t.Fatalf("enter events %v", types)
	}
	if code := alice.call(t, http.MethodPost, "/api/v1/interactions", map[string]string{"type": "USE_WORKSTATION", "object_key": "finance_pc_04"}, nil); code != 200 {
		t.Fatalf("use: %d", code)
	}
	types, fs := frames(t, ca, 2)
	if strings.Join(types, ",") != "asset.discovered,player.interacted" || fs[0]["data"].(map[string]any)["asset_code"] != "HQ-FIN-PC-04" {
		t.Fatalf("discovery events %v %v", types, fs[0])
	}

	// Level-up: durable notification + live events.
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE player_progress SET xp = 95 WHERE player_id = (SELECT p.id FROM players p JOIN users u ON u.id = p.user_id WHERE u.keycloak_subject = $1)`, alice.sub); err != nil {
		t.Fatal(err)
	}
	if code := alice.call(t, http.MethodPost, "/api/v1/interactions", map[string]string{"type": "INSPECT_OBJECT", "object_key": "finance_printer"}, nil); code != 200 {
		t.Fatalf("inspect: %d", code)
	}
	types, fs = frames(t, ca, 4)
	if strings.Join(types, ",") != "asset.discovered,career.level_up,notification.created,player.interacted" {
		t.Fatalf("level-up events %v", types)
	}
	note := fs[2]["data"].(map[string]any)
	if note["type"] != "career.promoted" || note["title"] != "Promoted to Security Analyst I" || !strings.Contains(note["body"].(string), "HQ Floor 3") {
		t.Fatalf("notification payload %v", note)
	}

	// Bob must not have received any of Alice's private events: the next
	// assertion checks that his first frame is the later world broadcast.

	// The notification is durable: listed over REST, can be marked read.
	var list struct {
		Data []notification.Notification `json:"data"`
		Meta struct {
			Unread int `json:"unread"`
		} `json:"meta"`
	}
	alice.call(t, http.MethodGet, "/api/v1/notifications?unread=true", nil, &list)
	if len(list.Data) != 1 || list.Meta.Unread != 1 || list.Data[0].Type != "career.promoted" {
		t.Fatalf("notifications %+v", list)
	}
	id := list.Data[0].ID.String()
	if code := bob.call(t, http.MethodPost, "/api/v1/notifications/"+id+"/read", nil, nil); code != 404 {
		t.Fatalf("bob marked alice's notification: %d", code)
	}
	if code := alice.call(t, http.MethodPost, "/api/v1/notifications/"+id+"/read", nil, nil); code != 200 {
		t.Fatalf("mark read: %d", code)
	}
	alice.call(t, http.MethodGet, "/api/v1/notifications?unread=true", nil, &list)
	if len(list.Data) != 0 || list.Meta.Unread != 0 {
		t.Fatalf("after read %+v", list)
	}

	// Admin announcements reach everyone in the world; players cannot send them.
	if code := bob.call(t, http.MethodPost, "/api/v1/admin/announcements", map[string]string{"title": "fake"}, nil); code != 403 {
		t.Fatalf("player announcement: %d", code)
	}
	admin := s.user("ADMIN")
	if code := admin.call(t, http.MethodPost, "/api/v1/admin/announcements", map[string]string{"title": "Fire drill at 14:00", "body": "Use the stairs."}, nil); code != 202 {
		t.Fatalf("announcement: %d", code)
	}
	for name, c := range map[string]*ws.Conn{"alice": ca, "bob": cb} {
		if m := frame(t, c); m["type"] != "world.announcement" || m["data"].(map[string]any)["title"] != "Fire drill at 14:00" {
			t.Fatalf("%s: expected the announcement as the next frame (private events leaked?), got %v", name, m)
		}
	}
	var audits int
	if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_logs WHERE action = 'world.announcement_sent'`).Scan(&audits); err != nil || audits < 1 {
		t.Fatalf("announcement not audited: %d %v", audits, err)
	}
}

func enterBody(t *testing.T, pool *pgxpool.Pool, zone string) map[string]any {
	return map[string]any{"type": "ENTER_ZONE", "zone_id": zoneID(t, pool, zone)}
}

func TestRealtimeZoneSubscriptionsFollowAccess(t *testing.T) {
	s := newRealtimeStack(t)
	p := s.user("PLAYER")
	c, _ := p.connect(t)

	sub := func(code string) map[string]any {
		b, _ := json.Marshal(map[string]string{"type": "subscribe", "topic": "zone:" + zoneID(t, s.pool, code).String()})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.Write(ctx, ws.MessageText, b); err != nil {
			t.Fatal(err)
		}
		return frame(t, c)
	}
	if m := sub("HQ_FLOOR_2"); m["type"] != "session.subscribed" {
		t.Fatalf("accessible zone: %v", m)
	}
	if m := sub("HQ_FLOOR_3"); m["type"] != "session.error" || m["data"].(map[string]any)["code"] != "forbidden_topic" {
		t.Fatalf("rank-gated zone must be refused: %v", m)
	}

	// Staff may follow any zone of the world.
	staff := s.user("INSTRUCTOR")
	cs, w := staff.connect(t)
	if topics := w["data"].(map[string]any)["topics"].([]any); len(topics) != 1 {
		t.Fatalf("staff without a player profile get only the world topic: %v", topics)
	}
	b, _ := json.Marshal(map[string]string{"type": "subscribe", "topic": "zone:" + zoneID(t, s.pool, "HQ_SERVER_ROOM").String()})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := cs.Write(ctx, ws.MessageText, b); err != nil {
		t.Fatal(err)
	}
	if m := frame(t, cs); m["type"] != "session.subscribed" {
		t.Fatalf("staff zone subscription: %v", m)
	}
	// A zone ID from no world is refused even for staff.
	b, _ = json.Marshal(map[string]string{"type": "subscribe", "topic": "zone:" + uuid.NewString()})
	if err := cs.Write(ctx, ws.MessageText, b); err != nil {
		t.Fatal(err)
	}
	if m := frame(t, cs); m["type"] != "session.error" {
		t.Fatalf("unknown zone: %v", m)
	}
}

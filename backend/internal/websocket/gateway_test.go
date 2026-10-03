package websocket

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	ws "github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/Darkload9999/VORTECH/backend/internal/auth"
	"github.com/Darkload9999/VORTECH/backend/internal/config"
	"github.com/Darkload9999/VORTECH/backend/internal/event"
)

var (
	worldTopic  = event.WorldTopic(uuid.MustParse("00000000-0000-7000-8000-00000000c0de"))
	openZone    = uuid.MustParse("00000000-0000-7000-8000-0000000000a1")
	lockedZone  = uuid.MustParse("00000000-0000-7000-8000-0000000000a2")
	testTimeout = 3 * time.Second
)

type fakeAuthz struct{}

func (fakeAuthz) WorldTopic(context.Context) (string, error) { return worldTopic, nil }
func (fakeAuthz) CanSubscribeZone(_ context.Context, _ *auth.Principal, z uuid.UUID) (bool, error) {
	return z == openZone, nil
}

type capturePub struct {
	mu     sync.Mutex
	events []event.Event
}

func (c *capturePub) Publish(_ context.Context, es ...event.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, es...)
}

func (c *capturePub) types() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, e := range c.events {
		if e.Topic != "" {
			out = append(out, "LEAK:"+e.Topic)
		}
		out = append(out, e.Type)
	}
	return out
}

type harness struct {
	g   *Gateway
	srv *httptest.Server
	pub *capturePub
}

func testConfig() config.RealtimeConfig {
	return config.RealtimeConfig{
		AllowedOrigins: []string{"localhost:5173"}, MaxConnections: 100, MaxConnectionsPerUser: 3,
		MaxConnectionAge: time.Hour, PingInterval: time.Hour, TicketTTL: 30 * time.Second,
	}
}

func newHarness(t *testing.T, mutate func(*config.RealtimeConfig, *http.Server)) *harness {
	t.Helper()
	cfg := testConfig()
	pub := &capturePub{}
	g := NewGateway(cfg, NewTicketStore(cfg.TicketTTL), fakeAuthz{}, pub, slog.New(slog.DiscardHandler))
	srv := httptest.NewUnstartedServer(http.HandlerFunc(g.Serve))
	if mutate != nil {
		mutate(&g.cfg, srv.Config)
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return &harness{g: g, srv: srv, pub: pub}
}

func principal() *auth.Principal {
	pid := uuid.New()
	return &auth.Principal{UserID: uuid.New(), PlayerID: &pid, Roles: auth.ParseRoles([]string{"PLAYER"})}
}

func (h *harness) ticket(t *testing.T, p *auth.Principal) string {
	t.Helper()
	tok, _, err := h.g.tickets.Issue(p)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (h *harness) dial(t *testing.T, ticket string, header http.Header) (*ws.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	url := "ws" + strings.TrimPrefix(h.srv.URL, "http") + "/ws?ticket=" + ticket
	return ws.Dial(ctx, url, &ws.DialOptions{HTTPHeader: header})
}

// connect dials and consumes the welcome frame.
func (h *harness) connect(t *testing.T, p *auth.Principal) (*ws.Conn, map[string]any) {
	t.Helper()
	c, _, err := h.dial(t, h.ticket(t, p), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	welcome := read(t, c)
	if welcome["type"] != typeWelcome {
		t.Fatalf("first frame = %v", welcome)
	}
	return c, welcome
}

func read(t *testing.T, c *ws.Conn) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	_, b, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("frame is not JSON: %s", b)
	}
	return m
}

func send(t *testing.T, c *ws.Conn, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if err := c.Write(ctx, ws.MessageText, b); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// closeStatus reads until the server closes and returns the status code.
func closeStatus(t *testing.T, c *ws.Conn) ws.StatusCode {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	for {
		if _, _, err := c.Read(ctx); err != nil {
			return ws.CloseStatus(err)
		}
	}
}

func waitConnections(t *testing.T, g *Gateway, n int) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for g.Connections() != n {
		if time.Now().After(deadline) {
			t.Fatalf("connections = %d, want %d", g.Connections(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestTicketStore(t *testing.T) {
	s := NewTicketStore(time.Minute)
	now := time.Unix(1000, 0)
	s.now = func() time.Time { return now }
	p := principal()

	tok, _, err := s.Issue(p)
	if err != nil || len(tok) < 40 {
		t.Fatalf("issue: %q %v", tok, err)
	}
	got, err := s.Redeem(tok)
	if err != nil || got.UserID != p.UserID {
		t.Fatalf("redeem: %v", err)
	}
	if _, err := s.Redeem(tok); !errors.Is(err, errInvalidTicket) {
		t.Fatal("ticket must be single-use")
	}

	tok, _, _ = s.Issue(p)
	now = now.Add(2 * time.Minute)
	if _, err := s.Redeem(tok); !errors.Is(err, errInvalidTicket) {
		t.Fatal("expired ticket accepted")
	}

	for range maxTicketsPerUser {
		if _, _, err := s.Issue(p); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.Issue(p); !errors.Is(err, errTooManyTicket) {
		t.Fatal("outstanding ticket cap not enforced")
	}
	now = now.Add(2 * time.Minute) // expiry frees the quota
	if _, _, err := s.Issue(p); err != nil {
		t.Fatalf("expired tickets should free the quota: %v", err)
	}
}

func TestUnauthenticatedConnectionsAreRejected(t *testing.T) {
	h := newHarness(t, nil)
	for name, tok := range map[string]string{"missing": "", "forged": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"} {
		_, resp, err := h.dial(t, tok, nil)
		if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s ticket: err=%v resp=%v", name, err, resp)
		}
	}
	// Replay of a used ticket.
	tok := h.ticket(t, principal())
	c, _, err := h.dial(t, tok, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	if _, resp, err := h.dial(t, tok, nil); err == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatal("replayed ticket accepted")
	}
}

func TestCrossOriginRejected(t *testing.T) {
	h := newHarness(t, nil)
	_, resp, err := h.dial(t, h.ticket(t, principal()), http.Header{"Origin": {"https://evil.example"}})
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin handshake: err=%v resp=%v", err, resp)
	}
	c, _, err := h.dial(t, h.ticket(t, principal()), http.Header{"Origin": {"http://localhost:5173"}})
	if err != nil {
		t.Fatalf("allowed origin rejected: %v", err)
	}
	c.CloseNow()
}

func TestWelcomeAndPrivateDelivery(t *testing.T) {
	h := newHarness(t, nil)
	alice, bob := principal(), principal()
	ca, welcome := h.connect(t, alice)
	cb, _ := h.connect(t, bob)

	data := welcome["data"].(map[string]any)
	topics := data["topics"].([]any)
	if len(topics) != 2 || topics[0] != event.PlayerTopic(*alice.PlayerID) || topics[1] != worldTopic {
		t.Fatalf("welcome topics = %v", topics)
	}

	ctx := context.Background()
	h.g.Deliver(ctx, event.Must(event.AssetDiscovered, event.PlayerTopic(*alice.PlayerID), map[string]string{"asset_code": "HQ-FIN-PC-04"}))
	h.g.Deliver(ctx, event.Must(event.PlayerConnected, "", map[string]string{"internal": "yes"}))
	h.g.Deliver(ctx, event.Must(event.WorldAnnouncement, worldTopic, map[string]string{"title": "Fire drill"}))

	if m := read(t, ca); m["type"] != event.AssetDiscovered || m["topic"] != event.PlayerTopic(*alice.PlayerID) {
		t.Fatalf("alice got %v", m)
	}
	if m := read(t, ca); m["type"] != event.WorldAnnouncement {
		t.Fatalf("alice second frame %v", m)
	}
	// Bob receives only the world event: never Alice's private event or
	// internal events.
	if m := read(t, cb); m["type"] != event.WorldAnnouncement {
		t.Fatalf("bob received %v (private or internal event leaked)", m)
	}
}

func TestSubscriptionAuthorization(t *testing.T) {
	h := newHarness(t, nil)
	p := principal()
	c, _ := h.connect(t, p)

	cases := []struct {
		topic, wantType, wantCode string
	}{
		{"zone:" + openZone.String(), typeSubscribed, ""},
		{"zone:" + lockedZone.String(), typeError, "forbidden_topic"},
		{"zone:not-a-uuid", typeError, "invalid_topic"},
		{"player:" + uuid.NewString(), typeError, "forbidden_topic"},
		{"world:" + uuid.NewString(), typeError, "forbidden_topic"},
		{"anything", typeError, "forbidden_topic"},
	}
	for _, tc := range cases {
		send(t, c, map[string]string{"type": "subscribe", "topic": tc.topic})
		m := read(t, c)
		if m["type"] != tc.wantType {
			t.Fatalf("subscribe %s: got %v", tc.topic, m)
		}
		if tc.wantCode != "" && m["data"].(map[string]any)["code"] != tc.wantCode {
			t.Fatalf("subscribe %s: code %v", tc.topic, m["data"])
		}
	}

	h.g.Deliver(context.Background(), event.Must("zone.updated", event.ZoneTopic(openZone), nil))
	h.g.Deliver(context.Background(), event.Must("zone.updated", event.ZoneTopic(lockedZone), map[string]bool{"secret": true}))
	if m := read(t, c); m["topic"] != event.ZoneTopic(openZone) {
		t.Fatalf("expected open zone event, got %v", m)
	}

	send(t, c, map[string]string{"type": "unsubscribe", "topic": "zone:" + openZone.String()})
	if m := read(t, c); m["type"] != typeUnsubscribed {
		t.Fatalf("unsubscribe: %v", m)
	}
	send(t, c, map[string]string{"type": "ping"})
	if m := read(t, c); m["type"] != typePong {
		t.Fatalf("ping: %v", m)
	}
	send(t, c, map[string]string{"type": "teleport"})
	if m := read(t, c); m["data"].(map[string]any)["code"] != "unknown_type" {
		t.Fatalf("unknown type: %v", m)
	}
}

func TestConnectionOutlivesServerTimeouts(t *testing.T) {
	// Regression: http.Server read/write deadlines survive the hijack and
	// must be cleared, or long-lived connections are cut off.
	h := newHarness(t, func(_ *config.RealtimeConfig, s *http.Server) {
		s.ReadTimeout, s.WriteTimeout = 200*time.Millisecond, 200*time.Millisecond
	})
	p := principal()
	c, _ := h.connect(t, p)
	time.Sleep(600 * time.Millisecond)
	h.g.Deliver(context.Background(), event.Must("still.alive", event.PlayerTopic(*p.PlayerID), nil))
	if m := read(t, c); m["type"] != "still.alive" {
		t.Fatalf("connection did not survive past server timeouts: %v", m)
	}
}

func TestPerUserConnectionLimit(t *testing.T) {
	h := newHarness(t, func(cfg *config.RealtimeConfig, _ *http.Server) { cfg.MaxConnectionsPerUser = 1 })
	p := principal()
	h.connect(t, p)
	_, resp, err := h.dial(t, h.ticket(t, p), nil)
	if err == nil || resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second connection: err=%v resp=%v", err, resp)
	}
	// Other users are unaffected.
	h.connect(t, principal())
}

func TestOversizedMessageClosesConnection(t *testing.T) {
	h := newHarness(t, nil)
	c, _ := h.connect(t, principal())
	send(t, c, map[string]string{"type": "subscribe", "topic": strings.Repeat("x", maxMessageBytes+10)})
	if code := closeStatus(t, c); code != ws.StatusMessageTooBig {
		t.Fatalf("close status = %v", code)
	}
}

func TestMessageFloodClosesConnection(t *testing.T) {
	h := newHarness(t, nil)
	c, _ := h.connect(t, principal())
	for range clientMsgBurst * 3 {
		b, _ := json.Marshal(map[string]string{"type": "ping"})
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		err := c.Write(ctx, ws.MessageText, b)
		cancel()
		if err != nil {
			break
		}
	}
	if code := closeStatus(t, c); code != ws.StatusPolicyViolation {
		t.Fatalf("close status = %v", code)
	}
}

func TestSlowConsumerIsDisconnected(t *testing.T) {
	g := NewGateway(testConfig(), NewTicketStore(time.Minute), fakeAuthz{}, nil, slog.New(slog.DiscardHandler))
	c := &client{id: uuid.New(), g: g, p: *principal(), send: make(chan []byte, 2), topics: map[string]struct{}{}, closed: make(chan struct{})}
	g.register(c, []string{"player:x"})
	for range 3 {
		g.Deliver(context.Background(), event.Must("x.y", "player:x", nil))
	}
	select {
	case <-c.closed:
		if c.closeCode != CloseSlowConsumer {
			t.Fatalf("close code = %v", c.closeCode)
		}
	default:
		t.Fatal("slow consumer was not disconnected")
	}
}

func TestDrainClosesConnectionsAndRejectsNewOnes(t *testing.T) {
	h := newHarness(t, nil)
	c, _ := h.connect(t, principal())
	h.g.Drain()
	if code := closeStatus(t, c); code != ws.StatusGoingAway {
		t.Fatalf("close status = %v", code)
	}
	if _, resp, err := h.dial(t, h.ticket(t, principal()), nil); err == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("connection during drain: err=%v resp=%v", err, resp)
	}
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if err := h.g.Wait(ctx); err != nil {
		t.Fatalf("handlers did not finish: %v", err)
	}
	if h.g.Connections() != 0 {
		t.Fatal("connections left after drain")
	}
}

func TestMaxConnectionAge(t *testing.T) {
	h := newHarness(t, func(cfg *config.RealtimeConfig, _ *http.Server) { cfg.MaxConnectionAge = 300 * time.Millisecond })
	c, _ := h.connect(t, principal())
	if code := closeStatus(t, c); code != CloseSessionExpired {
		t.Fatalf("close status = %v", code)
	}
}

func TestPresenceEventsAreInternal(t *testing.T) {
	h := newHarness(t, nil)
	c, _ := h.connect(t, principal())
	c.Close(ws.StatusNormalClosure, "bye")
	waitConnections(t, h.g, 0)
	got := strings.Join(h.pub.types(), ",")
	if got != event.PlayerConnected+","+event.PlayerDisconnected {
		t.Fatalf("presence events = %s (must be internal, without topic)", got)
	}
}

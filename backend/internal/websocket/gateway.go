package websocket

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	ws "github.com/coder/websocket"
	"github.com/google/uuid"
	"golang.org/x/time/rate"

	"github.com/Darkload9999/VORTECH/backend/internal/auth"
	"github.com/Darkload9999/VORTECH/backend/internal/config"
	"github.com/Darkload9999/VORTECH/backend/internal/event"
	"github.com/Darkload9999/VORTECH/backend/internal/httpx"
)

// Error codes returned before the upgrade.
const (
	CodeInvalidTicket      = "INVALID_TICKET"
	CodeTooManyConnections = "TOO_MANY_CONNECTIONS"
	CodeDraining           = "SERVICE_DRAINING"
)

// Authorizer answers the gateway's world-scoped questions.
type Authorizer interface {
	// WorldTopic is the active world's topic, or "" if no world is active.
	WorldTopic(ctx context.Context) (string, error)
	// CanSubscribeZone reports whether p may follow events of zoneID.
	CanSubscribeZone(ctx context.Context, p *auth.Principal, zoneID uuid.UUID) (bool, error)
}

// Gateway is the authenticated WebSocket gateway. It fans bus events out
// to the connections subscribed to their topic.
type Gateway struct {
	cfg     config.RealtimeConfig
	log     *slog.Logger
	tickets *TicketStore
	authz   Authorizer
	pub     event.Publisher

	mu       sync.RWMutex
	clients  map[*client]struct{}
	perUser  map[uuid.UUID]int
	topics   map[string]map[*client]struct{}
	draining bool

	wg sync.WaitGroup
}

// NewGateway returns a gateway. pub receives presence events.
func NewGateway(cfg config.RealtimeConfig, tickets *TicketStore, authz Authorizer, pub event.Publisher, log *slog.Logger) *Gateway {
	return &Gateway{
		cfg: cfg, log: log, tickets: tickets, authz: authz, pub: pub,
		clients: map[*client]struct{}{}, perUser: map[uuid.UUID]int{}, topics: map[string]map[*client]struct{}{},
	}
}

// IssueTicket serves POST /api/v1/ws/tickets (behind Authenticator.Protect).
func (g *Gateway) IssueTicket(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.FromContext(r.Context())
	if !ok {
		httpx.WriteError(w, r, http.StatusUnauthorized, httpx.CodeUnauthorized, "Authentication is required.")
		return
	}
	tok, exp, err := g.tickets.Issue(p)
	if errors.Is(err, errTooManyTicket) {
		httpx.WriteError(w, r, http.StatusTooManyRequests, httpx.CodeRateLimited, "Too many outstanding WebSocket tickets.")
		return
	}
	if err != nil {
		httpx.Fail(w, r, g.log, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{"ticket": tok, "expires_at": exp.UTC(), "path": "/ws"})
}

// Serve handles GET /ws?ticket=…: it authenticates with a one-time ticket,
// enforces connection limits, upgrades and runs the connection.
func (g *Gateway) Serve(w http.ResponseWriter, r *http.Request) {
	if g.isDraining() {
		httpx.WriteError(w, r, http.StatusServiceUnavailable, CodeDraining, "Instance is shutting down; reconnect.")
		return
	}
	p, err := g.tickets.Redeem(r.URL.Query().Get("ticket"))
	if err != nil {
		httpx.WriteError(w, r, http.StatusUnauthorized, CodeInvalidTicket, "A valid, unused WebSocket ticket is required.")
		return
	}
	switch err := g.reserve(p.UserID); {
	case errors.Is(err, errDraining):
		httpx.WriteError(w, r, http.StatusServiceUnavailable, CodeDraining, "Instance is shutting down; reconnect.")
		return
	case errors.Is(err, errAtCapacity):
		httpx.WriteError(w, r, http.StatusServiceUnavailable, CodeTooManyConnections, "The server has reached its connection limit; retry shortly.")
		return
	case errors.Is(err, errUserLimit):
		httpx.WriteError(w, r, http.StatusTooManyRequests, CodeTooManyConnections, "Too many open connections for this account.")
		return
	}
	defer g.wg.Done() // Add happened in reserve, under the drain lock
	reserved := true
	defer func() {
		if reserved {
			g.release(p.UserID)
		}
	}()

	worldTopic, err := g.authz.WorldTopic(r.Context())
	if err != nil {
		httpx.Fail(w, r, g.log, err)
		return
	}

	// http.Server's ReadTimeout/WriteTimeout deadlines survive the hijack;
	// clear them or every connection would die after a few seconds.
	rc := http.NewResponseController(w)
	if err := rc.SetReadDeadline(time.Time{}); err != nil {
		httpx.Fail(w, r, g.log, err)
		return
	}
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		httpx.Fail(w, r, g.log, err)
		return
	}

	conn, err := ws.Accept(w, r, &ws.AcceptOptions{OriginPatterns: g.cfg.AllowedOrigins})
	if err != nil {
		g.log.InfoContext(r.Context(), "websocket upgrade rejected", "error", err)
		return // Accept has written the HTTP error
	}
	conn.SetReadLimit(maxMessageBytes)

	// The request context is cancelled when the handler returns; the
	// connection gets its own, keeping request-scoped values for logging.
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer cancel()
	c := newClient(g, conn, p)

	topics := []string{}
	if p.PlayerID != nil {
		topics = append(topics, event.PlayerTopic(*p.PlayerID))
	}
	if worldTopic != "" {
		topics = append(topics, worldTopic)
	}
	g.register(c, topics)
	reserved = false
	defer g.unregister(c)

	g.publishPresence(ctx, event.PlayerConnected, c)
	defer g.publishPresence(context.WithoutCancel(ctx), event.PlayerDisconnected, c)

	expires := time.Now().Add(g.cfg.MaxConnectionAge)
	expiry := time.AfterFunc(g.cfg.MaxConnectionAge, func() {
		c.shutdown(CloseSessionExpired, "session expired; reconnect with a new ticket")
	})
	defer expiry.Stop()

	c.sendControl(typeWelcome, map[string]any{
		"connection_id":              c.id,
		"topics":                     topics,
		"heartbeat_interval_seconds": int(g.cfg.PingInterval / time.Second),
		"expires_at":                 expires.UTC(),
	})

	writerDone := make(chan struct{})
	go func() { defer close(writerDone); c.writeLoop(ctx) }()
	c.readLoop(ctx)
	c.shutdown(ws.StatusNormalClosure, "")
	<-writerDone
}

// Deliver is the event-bus handler: it fans an event out to subscribers
// of its topic. Events without a topic are internal and never delivered.
func (g *Gateway) Deliver(_ context.Context, e event.Event) {
	if e.Topic == "" {
		return
	}
	frame, err := json.Marshal(e)
	if err != nil {
		g.log.Error("encode event for websocket", "type", e.Type, "error", err)
		return
	}
	var slow []*client
	g.mu.RLock()
	for c := range g.topics[e.Topic] {
		if !c.enqueue(frame) {
			slow = append(slow, c)
		}
	}
	g.mu.RUnlock()
	for _, c := range slow {
		g.log.Warn("closing slow websocket consumer", "connection_id", c.id, "user_id", c.p.UserID)
		c.shutdown(CloseSlowConsumer, "too slow to keep up; reconnect")
	}
}

// Drain stops accepting connections and closes existing ones with
// "going away" so clients reconnect (to another instance if available).
func (g *Gateway) Drain() {
	g.mu.Lock()
	g.draining = true
	clients := make([]*client, 0, len(g.clients))
	for c := range g.clients {
		clients = append(clients, c)
	}
	g.mu.Unlock()
	for _, c := range clients {
		c.shutdown(ws.StatusGoingAway, "server restarting; reconnect")
	}
}

// Wait blocks until every connection handler has returned, bounded by ctx.
func (g *Gateway) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() { g.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Connections returns the number of open connections.
func (g *Gateway) Connections() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.clients)
}

func (g *Gateway) isDraining() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.draining
}

var (
	errDraining   = errors.New("gateway is draining")
	errAtCapacity = errors.New("websocket connection capacity reached")
	errUserLimit  = errors.New("per-user websocket connection limit reached")
)

// reserve claims a connection slot for user (released by unregister or,
// if the upgrade fails, by release) and registers the handler with the
// wait group. Both happen under the lock Drain takes, so no handler can
// start after draining began and Add never races with Wait.
func (g *Gateway) reserve(user uuid.UUID) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.draining {
		return errDraining
	}
	total := 0
	for _, n := range g.perUser {
		total += n
	}
	switch {
	case total >= g.cfg.MaxConnections:
		return errAtCapacity
	case g.perUser[user] >= g.cfg.MaxConnectionsPerUser:
		return errUserLimit
	}
	g.perUser[user]++
	g.wg.Add(1)
	return nil
}

func (g *Gateway) release(user uuid.UUID) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.perUser[user]--; g.perUser[user] <= 0 {
		delete(g.perUser, user)
	}
}

func (g *Gateway) register(c *client, topics []string) {
	g.mu.Lock()
	g.clients[c] = struct{}{}
	for _, t := range topics {
		g.subscribeLocked(c, t)
	}
	draining := g.draining
	g.mu.Unlock()
	if draining {
		// Upgraded while Drain ran: close it like the others.
		c.shutdown(ws.StatusGoingAway, "server restarting; reconnect")
	}
}

func (g *Gateway) unregister(c *client) {
	g.mu.Lock()
	delete(g.clients, c)
	for t := range c.topics {
		g.unsubscribeLocked(c, t)
	}
	g.mu.Unlock()
	g.release(c.p.UserID)
}

func (g *Gateway) subscribeLocked(c *client, topic string) {
	set, ok := g.topics[topic]
	if !ok {
		set = map[*client]struct{}{}
		g.topics[topic] = set
	}
	set[c] = struct{}{}
	c.topics[topic] = struct{}{}
}

func (g *Gateway) unsubscribeLocked(c *client, topic string) {
	if set, ok := g.topics[topic]; ok {
		delete(set, c)
		if len(set) == 0 {
			delete(g.topics, topic)
		}
	}
	delete(c.topics, topic)
}

func (g *Gateway) publishPresence(ctx context.Context, typ string, c *client) {
	if g.pub == nil {
		return
	}
	// Presence is internal: no topic, never sent to other players.
	g.pub.Publish(ctx, event.Must(typ, "", map[string]any{
		"connection_id": c.id, "user_id": c.p.UserID, "player_id": c.p.PlayerID,
	}))
}

// client is one WebSocket connection.
type client struct {
	id      uuid.UUID
	g       *Gateway
	conn    *ws.Conn
	p       auth.Principal
	send    chan []byte
	topics  map[string]struct{} // guarded by g.mu
	limiter *rate.Limiter

	closed      chan struct{}
	closeOnce   sync.Once
	closeCode   ws.StatusCode
	closeReason string
}

func newClient(g *Gateway, conn *ws.Conn, p auth.Principal) *client {
	return &client{
		id: uuid.New(), g: g, conn: conn, p: p,
		send: make(chan []byte, sendBuffer), topics: map[string]struct{}{},
		limiter: rate.NewLimiter(clientMsgPerSec, clientMsgBurst),
		closed:  make(chan struct{}),
	}
}

func (c *client) enqueue(frame []byte) bool {
	select {
	case <-c.closed:
		return true // closing anyway; not a slow consumer
	default:
	}
	select {
	case c.send <- frame:
		return true
	default:
		return false
	}
}

// shutdown records why the connection ends; the write loop performs the
// close handshake. The first reason wins.
func (c *client) shutdown(code ws.StatusCode, reason string) {
	c.closeOnce.Do(func() {
		c.closeCode, c.closeReason = code, reason
		close(c.closed)
	})
}

func (c *client) sendControl(typ string, data any) {
	frame, err := json.Marshal(event.Must(typ, "", data))
	if err == nil && !c.enqueue(frame) {
		c.shutdown(CloseSlowConsumer, "too slow to keep up; reconnect")
	}
}

func (c *client) sendError(code, msg string) {
	c.sendControl(typeError, map[string]string{"code": code, "message": msg})
}

func (c *client) writeLoop(ctx context.Context) {
	ping := time.NewTicker(c.g.cfg.PingInterval)
	defer ping.Stop()
	timeout := writeTimeoutSecs * time.Second
	for {
		select {
		case <-c.closed:
			_ = c.conn.Close(c.closeCode, c.closeReason)
			return
		case frame := <-c.send:
			wctx, cancel := context.WithTimeout(ctx, timeout)
			err := c.conn.Write(wctx, ws.MessageText, frame)
			cancel()
			if err != nil {
				c.shutdown(ws.StatusGoingAway, "write failed")
				_ = c.conn.CloseNow()
				return
			}
		case <-ping.C:
			// Ping waits for the pong (processed by the read loop): a dead
			// peer is detected within one interval plus the timeout.
			pctx, cancel := context.WithTimeout(ctx, timeout)
			err := c.conn.Ping(pctx)
			cancel()
			if err != nil {
				c.shutdown(ws.StatusGoingAway, "heartbeat timeout")
				_ = c.conn.CloseNow()
				return
			}
		}
	}
}

// readLoop reads client frames until the connection closes. It must not
// cancel its read context on shutdown: cancelling a Read closes the
// connection abruptly, racing the write loop's close handshake. Instead
// the write loop's Close completes the handshake and Read then returns.
func (c *client) readLoop(ctx context.Context) {
	for {
		typ, data, err := c.conn.Read(ctx)
		if err != nil {
			return
		}
		if !c.limiter.Allow() {
			c.g.log.Warn("websocket client exceeded message rate", "connection_id", c.id, "user_id", c.p.UserID)
			c.shutdown(ws.StatusPolicyViolation, "message rate limit exceeded")
			return
		}
		if typ != ws.MessageText {
			c.sendError("invalid_message", "Only JSON text messages are accepted.")
			continue
		}
		var msg clientMessage
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&msg); err != nil {
			c.sendError("invalid_message", "Message must be a JSON object with type and optional topic.")
			continue
		}
		c.handle(ctx, msg)
	}
}

func (c *client) handle(ctx context.Context, msg clientMessage) {
	switch msg.Type {
	case "ping":
		c.sendControl(typePong, map[string]any{})
	case "subscribe":
		c.subscribe(ctx, msg.Topic)
	case "unsubscribe":
		c.g.mu.Lock()
		_, had := c.topics[msg.Topic]
		if had {
			c.g.unsubscribeLocked(c, msg.Topic)
		}
		c.g.mu.Unlock()
		if !had {
			c.sendError("not_subscribed", "Not subscribed to that topic.")
			return
		}
		c.sendControl(typeUnsubscribed, map[string]string{"topic": msg.Topic})
	default:
		c.sendError("unknown_type", "Supported message types: subscribe, unsubscribe, ping.")
	}
}

// subscribe authorises and adds a topic. Only zone topics can be added on
// request: private player topics and the world topic are assigned by the
// server at connect time and can never be requested for someone else.
func (c *client) subscribe(ctx context.Context, topic string) {
	rest, ok := strings.CutPrefix(topic, "zone:")
	if !ok {
		c.sendError("forbidden_topic", "Only zone:<id> topics can be subscribed to.")
		return
	}
	zoneID, err := uuid.Parse(rest)
	if err != nil {
		c.sendError("invalid_topic", "Zone topic must be zone:<uuid>.")
		return
	}
	c.g.mu.RLock()
	_, already := c.topics[topic]
	count := len(c.topics)
	c.g.mu.RUnlock()
	switch {
	case already:
		c.sendControl(typeSubscribed, map[string]string{"topic": topic})
		return
	case count >= maxTopics:
		c.sendError("too_many_topics", "Topic limit reached.")
		return
	}

	allowed, err := c.g.authz.CanSubscribeZone(ctx, &c.p, zoneID)
	if err != nil {
		c.g.log.Error("authorise zone subscription", "error", err, "connection_id", c.id)
		c.sendError("internal_error", "Could not authorise the subscription.")
		return
	}
	if !allowed {
		c.sendError("forbidden_topic", "You cannot follow that zone.")
		return
	}
	c.g.mu.Lock()
	c.g.subscribeLocked(c, topic)
	c.g.mu.Unlock()
	c.sendControl(typeSubscribed, map[string]string{"topic": topic})
}

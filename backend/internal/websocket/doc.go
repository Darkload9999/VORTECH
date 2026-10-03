// Package websocket is the authenticated real-time gateway (GET /ws).
//
// Clients obtain a single-use ticket from POST /api/v1/ws/tickets and
// connect with /ws?ticket=…. Each connection is subscribed to its player's
// private topic and the world topic; zone topics may be added if the
// caller can access the zone. Events from the internal bus are fanned out
// per topic. Connections have heartbeats, message size and rate limits,
// per-user and global caps, a bounded send queue (slow consumers are
// disconnected rather than buffered without limit) and a maximum age after
// which the client reconnects with a fresh ticket.
//
// Private player events are only ever delivered to that player's own
// connections. Anything a client must not miss is persisted and re-read
// over REST after reconnecting.
package websocket

package websocket

import (
	ws "github.com/coder/websocket"
)

// Wire protocol.
//
// Server → client frames share the event envelope:
//
//	{"id": "...", "type": "range.ready", "topic": "player:...", "timestamp": "...", "data": {...}}
//
// Control frames use "session.*" types (welcome, subscribed, unsubscribed,
// pong, error) and carry no topic.
//
// Client → server frames:
//
//	{"type": "subscribe",   "topic": "zone:<uuid>"}
//	{"type": "unsubscribe", "topic": "zone:<uuid>"}
//	{"type": "ping"}
//
// The player's private topic and the world topic are subscribed
// automatically; only zone topics can be added, and only for zones the
// caller may access.

// Control frame types.
const (
	typeWelcome      = "session.welcome"
	typeSubscribed   = "session.subscribed"
	typeUnsubscribed = "session.unsubscribed"
	typePong         = "session.pong"
	typeError        = "session.error"
)

// Limits.
const (
	maxMessageBytes  = 4 << 10
	maxTopics        = 20
	sendBuffer       = 256
	clientMsgPerSec  = 10
	clientMsgBurst   = 20
	writeTimeoutSecs = 10
)

// Close codes. 4000–4999 are application-defined.
const (
	// CloseSessionExpired asks the client to reconnect with a new ticket.
	CloseSessionExpired ws.StatusCode = 4001
	// CloseSlowConsumer: the client did not keep up with its messages.
	CloseSlowConsumer = ws.StatusTryAgainLater
)

// clientMessage is a frame sent by the client.
type clientMessage struct {
	Type  string `json:"type"`
	Topic string `json:"topic,omitempty"`
}

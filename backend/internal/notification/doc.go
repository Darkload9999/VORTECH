// Package notification stores durable per-user notifications and serves
// them over REST. Creation happens inside the transaction that caused the
// notification; the returned notification.created event is published to
// the user's private WebSocket topic after commit.
package notification

package event

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Darkload9999/VORTECH/backend/internal/database"
)

// Notify sends events to the API processes through PostgreSQL. Processes
// that have no bus subscribers of their own (the worker) use it; ex is the
// transaction that produced the events, so they are delivered exactly when
// it commits and never for rolled-back work.
func Notify(ctx context.Context, ex database.Execer, events ...Event) error {
	for _, e := range events {
		raw, err := json.Marshal(e)
		if err != nil {
			return fmt.Errorf("encode %s event: %w", e.Type, err)
		}
		if err := database.Notify(ctx, ex, database.ChannelEvents, string(raw)); err != nil {
			return err
		}
	}
	return nil
}

// Bridge republishes events received through PostgreSQL on pub until ctx
// ends. Like the bus itself it is at-most-once: events sent while the
// listener reconnects are lost, and clients recover durable state over REST.
func Bridge(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger, pub Publisher) {
	database.Listen(ctx, pool, log, []string{database.ChannelEvents}, nil, func(n *pgconn.Notification) {
		var e Event
		if err := json.Unmarshal([]byte(n.Payload), &e); err != nil || e.Type == "" {
			log.Warn("discarding malformed event notification", "error", err)
			return
		}
		pub.Publish(ctx, e)
	})
}

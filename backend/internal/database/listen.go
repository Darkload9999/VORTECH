package database

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Notification channels used with pg_notify.
const (
	// ChannelEvents carries JSON-encoded events from any process to the
	// API's event bus (see event.Notify).
	ChannelEvents = "vortech_events"
	// ChannelJobs wakes workers when a job is enqueued.
	ChannelJobs = "vortech_jobs"
)

// MaxNotifyPayload is PostgreSQL's limit on a NOTIFY payload (8000 bytes
// minus a safety margin).
const MaxNotifyPayload = 7900

// Execer is satisfied by pgx.Tx, *pgx.Conn and *pgxpool.Pool.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Notify sends a notification through ex, usually the transaction that
// produced it: PostgreSQL delivers it only if that transaction commits.
func Notify(ctx context.Context, ex Execer, channel, payload string) error {
	if len(payload) > MaxNotifyPayload {
		return fmt.Errorf("notify %s: payload of %d bytes exceeds %d", channel, len(payload), MaxNotifyPayload)
	}
	if _, err := ex.Exec(ctx, "SELECT pg_notify($1, $2)", channel, payload); err != nil {
		return fmt.Errorf("notify %s: %w", channel, err)
	}
	return nil
}

// Listen holds one pooled connection, LISTENs on channels and calls fn for
// each notification until ctx ends. Lost connections are re-established
// with backoff; onConnect (optional) runs after every (re)connect so the
// caller can catch up on anything sent while it was not listening.
func Listen(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger, channels []string, onConnect func(), fn func(*pgconn.Notification)) {
	backoff := 250 * time.Millisecond
	for ctx.Err() == nil {
		err := listenOnce(ctx, pool, channels, func() {
			backoff = 250 * time.Millisecond
			if onConnect != nil {
				onConnect()
			}
		}, fn)
		if ctx.Err() != nil {
			return
		}
		log.Warn("postgres listener disconnected; reconnecting", "channels", channels, "error", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 10*time.Second)
	}
}

func listenOnce(ctx context.Context, pool *pgxpool.Pool, channels []string, connected func(), fn func(*pgconn.Notification)) error {
	pc, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire listener connection: %w", err)
	}
	// A LISTENing connection must never go back to the pool: take it over
	// and close it when done.
	conn := pc.Hijack()
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = conn.Close(closeCtx)
	}()

	for _, ch := range channels {
		if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{ch}.Sanitize()); err != nil {
			return fmt.Errorf("listen %s: %w", ch, err)
		}
	}
	connected()
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return ctx.Err()
			}
			return err
		}
		fn(n)
	}
}

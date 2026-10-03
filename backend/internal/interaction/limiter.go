package interaction

import (
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/time/rate"
)

// Limiter is a per-player token bucket. It is process-local; it moves to
// Valkey when the API runs as several replicas.
type Limiter struct {
	rate  rate.Limit
	burst int
	idle  time.Duration
	now   func() time.Time

	mu        sync.Mutex
	players   map[uuid.UUID]*bucket
	lastSweep time.Time
}

type bucket struct {
	lim      *rate.Limiter
	lastSeen time.Time
}

// NewLimiter allows perSecond sustained interactions with the given burst.
func NewLimiter(perSecond float64, burst int) *Limiter {
	return &Limiter{
		rate: rate.Limit(perSecond), burst: burst, idle: 10 * time.Minute, now: time.Now,
		players: map[uuid.UUID]*bucket{},
	}
}

// Allow reports whether player may perform another interaction now.
func (l *Limiter) Allow(player uuid.UUID) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if now.Sub(l.lastSweep) > l.idle {
		for id, b := range l.players {
			if now.Sub(b.lastSeen) > l.idle {
				delete(l.players, id)
			}
		}
		l.lastSweep = now
	}
	b, ok := l.players[player]
	if !ok {
		b = &bucket{lim: rate.NewLimiter(l.rate, l.burst)}
		l.players[player] = b
	}
	b.lastSeen = now
	return b.lim.AllowN(now, 1)
}

package event

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
)

// DefaultBuffer is the per-subscription queue length.
const DefaultBuffer = 1024

// Bus is the in-memory Publisher/Subscriber. Each subscription has its own
// bounded queue and goroutine: a slow subscriber drops its own events
// (counted and logged) but never delays publishers or other subscribers.
type Bus struct {
	log    *slog.Logger
	buffer int

	mu     sync.RWMutex
	subs   map[*subscription]struct{}
	closed bool
	wg     sync.WaitGroup

	published atomic.Uint64
	dropped   atomic.Uint64
}

type subscription struct {
	name   string
	filter func(Event) bool
	ch     chan Event
	stop   chan struct{}
	once   sync.Once
}

// NewBus returns an empty bus.
func NewBus(log *slog.Logger) *Bus {
	return &Bus{log: log, buffer: DefaultBuffer, subs: map[*subscription]struct{}{}}
}

// Publish implements Publisher.
func (b *Bus) Publish(ctx context.Context, events ...Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return
	}
	for _, e := range events {
		b.published.Add(1)
		for s := range b.subs {
			if s.filter != nil && !s.filter(e) {
				continue
			}
			select {
			case s.ch <- e:
			default:
				b.dropped.Add(1)
				b.log.WarnContext(ctx, "event dropped: subscriber queue full", "subscriber", s.name, "type", e.Type)
			}
		}
	}
}

// Subscribe implements Subscriber.
func (b *Bus) Subscribe(name string, filter func(Event) bool, h Handler) func() {
	s := &subscription{name: name, filter: filter, ch: make(chan Event, b.buffer), stop: make(chan struct{})}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return func() {}
	}
	b.subs[s] = struct{}{}
	b.wg.Add(1)
	b.mu.Unlock()

	go func() {
		defer b.wg.Done()
		ctx := context.Background()
		for {
			select {
			case <-s.stop:
				return
			case e := <-s.ch:
				b.dispatch(ctx, s, h, e)
			}
		}
	}()

	return func() {
		b.mu.Lock()
		delete(b.subs, s)
		b.mu.Unlock()
		s.once.Do(func() { close(s.stop) })
	}
}

// dispatch runs a handler, containing panics so one faulty subscriber
// cannot take down the process.
func (b *Bus) dispatch(ctx context.Context, s *subscription, h Handler, e Event) {
	defer func() {
		if r := recover(); r != nil {
			b.log.Error("event handler panicked", "subscriber", s.name, "type", e.Type, "panic", r)
		}
	}()
	h(ctx, e)
}

// Close stops accepting events and waits for subscription goroutines to
// exit (queued events are discarded), bounded by ctx.
func (b *Bus) Close(ctx context.Context) error {
	b.mu.Lock()
	b.closed = true
	for s := range b.subs {
		s.once.Do(func() { close(s.stop) })
	}
	b.subs = map[*subscription]struct{}{}
	b.mu.Unlock()

	done := make(chan struct{})
	go func() { b.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stats reports totals since start.
func (b *Bus) Stats() (published, dropped uint64) {
	return b.published.Load(), b.dropped.Load()
}

package event

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func newBus() *Bus { return NewBus(slog.New(slog.DiscardHandler)) }

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestNewEvent(t *testing.T) {
	p := uuid.New()
	e := Must(AssetDiscovered, PlayerTopic(p), map[string]any{"asset_code": "HQ-FIN-PC-04"})
	if e.ID.Version() != 7 || e.Type != AssetDiscovered || e.Topic != "player:"+p.String() {
		t.Fatalf("unexpected event %+v", e)
	}
	var wire map[string]any
	b, _ := json.Marshal(e)
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"id", "type", "topic", "timestamp", "data"} {
		if _, ok := wire[k]; !ok {
			t.Errorf("wire format missing %q: %s", k, b)
		}
	}
	if _, err := New("x.y", "", make(chan int)); err == nil {
		t.Fatal("unencodable payload must error")
	}
}

func TestPublishSubscribeWithFilter(t *testing.T) {
	b := newBus()
	defer b.Close(context.Background())

	var mu sync.Mutex
	var got []string
	b.Subscribe("test", func(e Event) bool { return e.Topic != "" }, func(_ context.Context, e Event) {
		mu.Lock()
		got = append(got, e.Type)
		mu.Unlock()
	})
	b.Publish(context.Background(),
		Must(PlayerInteracted, "player:x", nil),
		Must(PlayerConnected, "", nil), // internal: filtered out
		Must(AssetDiscovered, "player:x", nil),
	)
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(got) == 2 })
	mu.Lock()
	defer mu.Unlock()
	if got[0] != PlayerInteracted || got[1] != AssetDiscovered {
		t.Fatalf("delivery order/filter wrong: %v", got)
	}
}

func TestSlowSubscriberDropsInsteadOfBlocking(t *testing.T) {
	b := newBus()
	b.buffer = 2
	defer b.Close(context.Background())

	release := make(chan struct{})
	var fastCount atomic.Int32
	b.Subscribe("slow", nil, func(context.Context, Event) { <-release })
	b.Subscribe("fast", nil, func(context.Context, Event) { fastCount.Add(1) })

	start := time.Now()
	for range 50 {
		b.Publish(context.Background(), Must("x.y", "", nil))
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("publishing blocked on a slow subscriber")
	}
	close(release)
	if _, dropped := b.Stats(); dropped == 0 {
		t.Fatal("expected drops for the slow subscriber")
	}
	waitFor(t, func() bool { return fastCount.Load() >= 2 })
}

func TestUnsubscribeAndClose(t *testing.T) {
	b := newBus()
	var n atomic.Int32
	unsub := b.Subscribe("s", nil, func(context.Context, Event) { n.Add(1) })
	b.Publish(context.Background(), Must("a.b", "", nil))
	waitFor(t, func() bool { return n.Load() == 1 })
	unsub()
	unsub() // idempotent
	b.Publish(context.Background(), Must("a.b", "", nil))
	time.Sleep(20 * time.Millisecond)
	if n.Load() != 1 {
		t.Fatal("unsubscribed handler still receives events")
	}

	b.Subscribe("s2", nil, func(context.Context, Event) {})
	if err := b.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	b.Publish(context.Background(), Must("a.b", "", nil)) // no panic after close
	if unsub := b.Subscribe("late", nil, func(context.Context, Event) {}); unsub == nil {
		t.Fatal("subscribe after close must return a no-op")
	}
}

func TestHandlerPanicIsContained(t *testing.T) {
	b := newBus()
	defer b.Close(context.Background())
	var n atomic.Int32
	b.Subscribe("panicky", nil, func(_ context.Context, e Event) {
		n.Add(1)
		if e.Type == "boom.now" {
			panic("boom")
		}
	})
	b.Publish(context.Background(), Must("boom.now", "", nil), Must("ok.then", "", nil))
	waitFor(t, func() bool { return n.Load() == 2 })
}

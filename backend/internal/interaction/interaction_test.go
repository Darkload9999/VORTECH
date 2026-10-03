package interaction

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Darkload9999/VORTECH/backend/internal/httpx"
)

func TestRequestValidate(t *testing.T) {
	id := uuid.New()
	tests := []struct {
		name string
		req  Request
		ok   bool
	}{
		{"enter zone", Request{Type: EnterZone, ZoneID: &id}, true},
		{"leave zone", Request{Type: LeaveZone, ZoneID: &id}, true},
		{"use workstation", Request{Type: UseWorkstation, ObjectKey: "finance_pc_04"}, true},
		{"talk to npc", Request{Type: TalkToNPC, EmployeeID: &id}, true},
		{"unknown type", Request{Type: "TELEPORT", ZoneID: &id}, false},
		{"lowercase type", Request{Type: "enter_zone", ZoneID: &id}, false},
		{"zone type without zone", Request{Type: EnterZone}, false},
		{"zone type with extra target", Request{Type: EnterZone, ZoneID: &id, ObjectKey: "x"}, false},
		{"object type without key", Request{Type: OpenDoor}, false},
		{"object type with zone", Request{Type: OpenDoor, ObjectKey: "door", ZoneID: &id}, false},
		{"malformed object key", Request{Type: OpenDoor, ObjectKey: "../../etc/passwd"}, false},
		{"npc without employee", Request{Type: TalkToNPC}, false},
		{"npc with object", Request{Type: TalkToNPC, EmployeeID: &id, ObjectKey: "x"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Validate()
			if tt.ok && err != nil {
				t.Fatalf("unexpected error %v", err)
			}
			if !tt.ok {
				var apiErr *httpx.Error
				if !errors.As(err, &apiErr) || apiErr.Status != 400 || apiErr.Code != CodeInvalidInteraction {
					t.Fatalf("expected 400 INVALID_INTERACTION, got %v", err)
				}
			}
		})
	}
}

func TestEveryTypeHasATargetAndMessagesCoverReasons(t *testing.T) {
	for _, typ := range []string{EnterZone, LeaveZone, OpenDoor, CloseDoor, UseWorkstation, TalkToNPC,
		InspectObject, AccessTerminal, ReadDocument, StartEngagement} {
		if _, ok := targets[typ]; !ok {
			t.Errorf("%s has no target kind", typ)
		}
	}
	for _, r := range []string{ReasonNotInZone, ReasonNotSupported, ReasonNPCUnavailable, ReasonRequiresEngagement} {
		if messages[r] == "" {
			t.Errorf("reason %s has no message", r)
		}
	}
}

func TestLimiter(t *testing.T) {
	l := NewLimiter(1, 3)
	now := time.Unix(1000, 0)
	l.now = func() time.Time { return now }
	a, b := uuid.New(), uuid.New()

	for i := range 3 {
		if !l.Allow(a) {
			t.Fatalf("burst request %d denied", i)
		}
	}
	if l.Allow(a) {
		t.Fatal("request beyond burst allowed")
	}
	if !l.Allow(b) {
		t.Fatal("players must have independent buckets")
	}
	now = now.Add(time.Second)
	if !l.Allow(a) {
		t.Fatal("bucket did not refill")
	}

	// Idle buckets are swept.
	now = now.Add(time.Hour)
	l.Allow(b)
	if _, ok := l.players[a]; ok {
		t.Fatal("idle bucket was not swept")
	}
}

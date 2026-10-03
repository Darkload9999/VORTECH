package cyberrange

import (
	"fmt"
	"slices"
)

// State is a range lifecycle state.
type State string

// Lifecycle:
//
//	REQUESTED ─▶ PROVISIONING ─▶ STARTING ─▶ READY ─▶ ACTIVE
//	    │             │              │          │        │
//	    ▼             ▼              ▼          ▼        ▼
//	  QUEUED        FAILED ◀─────────┘       EXPIRED  EXPIRED
//	    │             │                         │        │
//	    └──────────▶ STOPPING ◀─────────────────┴────────┘ (and on request)
//	                  │
//	                  ▼
//	              DESTROYED
//
// QUEUED waits for capacity; FAILED and EXPIRED still hold resources until
// the destroy job removes them.
const (
	Requested    State = "REQUESTED"
	Queued       State = "QUEUED"
	Provisioning State = "PROVISIONING"
	Starting     State = "STARTING"
	Ready        State = "READY"
	Active       State = "ACTIVE"
	Stopping     State = "STOPPING"
	Destroyed    State = "DESTROYED"
	Failed       State = "FAILED"
	Expired      State = "EXPIRED"
)

var transitions = map[State][]State{
	Requested:    {Queued, Provisioning, Stopping, Failed},
	Queued:       {Provisioning, Stopping, Failed},
	Provisioning: {Starting, Failed, Stopping},
	Starting:     {Ready, Failed, Stopping, Expired},
	Ready:        {Active, Stopping, Expired, Failed},
	Active:       {Stopping, Expired, Failed},
	Expired:      {Stopping},
	Failed:       {Stopping},
	Stopping:     {Destroyed},
	Destroyed:    {},
}

// CanTransition reports whether from → to is a valid lifecycle step.
func CanTransition(from, to State) bool {
	return slices.Contains(transitions[from], to)
}

// CheckTransition returns an error for an invalid step.
func CheckTransition(from, to State) error {
	if !CanTransition(from, to) {
		return fmt.Errorf("invalid range transition %s → %s", from, to)
	}
	return nil
}

// Live states count toward the one-range-per-player limit.
func (s State) Live() bool {
	switch s {
	case Requested, Queued, Provisioning, Starting, Ready, Active:
		return true
	}
	return false
}

// ConsumesCapacity reports whether cluster resources are (or may be)
// allocated in this state.
func (s State) ConsumesCapacity() bool {
	switch s {
	case Provisioning, Starting, Ready, Active, Stopping, Failed, Expired:
		return true
	}
	return false
}

// Stoppable reports whether a player may ask to destroy the range now.
func (s State) Stoppable() bool {
	return CanTransition(s, Stopping)
}

// CapacityStates lists the states that consume capacity, for SQL queries.
func CapacityStates() []string {
	return []string{string(Provisioning), string(Starting), string(Ready), string(Active), string(Stopping), string(Failed), string(Expired)}
}

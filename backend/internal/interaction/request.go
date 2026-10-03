package interaction

import (
	"net/http"
	"regexp"

	"github.com/google/uuid"

	"github.com/Darkload9999/VORTECH/backend/internal/httpx"
)

// Interaction types.
const (
	EnterZone       = "ENTER_ZONE"
	LeaveZone       = "LEAVE_ZONE"
	OpenDoor        = "OPEN_DOOR"
	CloseDoor       = "CLOSE_DOOR"
	UseWorkstation  = "USE_WORKSTATION"
	TalkToNPC       = "TALK_TO_NPC"
	InspectObject   = "INSPECT_OBJECT"
	AccessTerminal  = "ACCESS_TERMINAL"
	ReadDocument    = "READ_DOCUMENT"
	StartEngagement = "START_ENGAGEMENT"
)

// CodeInvalidInteraction reports a malformed interaction request.
const CodeInvalidInteraction = "INVALID_INTERACTION"

type targetKind int

const (
	targetZone targetKind = iota
	targetObject
	targetEmployee
)

var targets = map[string]targetKind{
	EnterZone: targetZone, LeaveZone: targetZone,
	OpenDoor: targetObject, CloseDoor: targetObject, UseWorkstation: targetObject,
	InspectObject: targetObject, AccessTerminal: targetObject, ReadDocument: targetObject,
	StartEngagement: targetObject,
	TalkToNPC:       targetEmployee,
}

var objectKey = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,127}$`)

// Request is the body of POST /api/v1/interactions. Exactly the target
// field matching the type must be set. The client says only what it wants
// to do; whether it may is decided entirely by the server.
type Request struct {
	Type       string     `json:"type"`
	ZoneID     *uuid.UUID `json:"zone_id,omitempty"`
	ObjectKey  string     `json:"object_key,omitempty"`
	EmployeeID *uuid.UUID `json:"employee_id,omitempty"`
}

// Validate checks the request's shape (not whether it is permitted).
func (r Request) Validate() error {
	kind, ok := targets[r.Type]
	if !ok {
		return invalid("type must be one of ENTER_ZONE, LEAVE_ZONE, OPEN_DOOR, CLOSE_DOOR, USE_WORKSTATION, TALK_TO_NPC, INSPECT_OBJECT, ACCESS_TERMINAL, READ_DOCUMENT, START_ENGAGEMENT.")
	}
	hasZone, hasObject, hasEmployee := r.ZoneID != nil, r.ObjectKey != "", r.EmployeeID != nil
	switch kind {
	case targetZone:
		if !hasZone || hasObject || hasEmployee {
			return invalid(r.Type + " requires zone_id and no other target.")
		}
	case targetObject:
		if !hasObject || hasZone || hasEmployee {
			return invalid(r.Type + " requires object_key and no other target.")
		}
		if !objectKey.MatchString(r.ObjectKey) {
			return invalid("object_key has an invalid format.")
		}
	case targetEmployee:
		if !hasEmployee || hasZone || hasObject {
			return invalid(r.Type + " requires employee_id and no other target.")
		}
	}
	return nil
}

func invalid(msg string) error {
	return httpx.NewError(http.StatusBadRequest, CodeInvalidInteraction, msg)
}

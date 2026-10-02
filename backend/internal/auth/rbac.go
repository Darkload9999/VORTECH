package auth

import (
	"slices"
	"sort"
)

// Role is a platform role assigned in Keycloak (realm role). Platform roles
// govern what a person may do on the platform; game progression (unlocked
// floors, career level) is stored in PostgreSQL, never in Keycloak.
type Role string

// Platform roles.
const (
	RolePlayer          Role = "PLAYER"
	RoleInstructor      Role = "INSTRUCTOR"
	RoleScenarioCreator Role = "SCENARIO_CREATOR"
	RoleAdmin           Role = "ADMIN"
)

var knownRoles = []Role{RolePlayer, RoleInstructor, RoleScenarioCreator, RoleAdmin}

// Permission is a capability checked by handlers. Handlers depend on
// permissions, never on role names, so the role model can evolve without
// touching every endpoint.
type Permission string

// Permissions. "own" permissions additionally require a per-resource
// ownership check in the owning module.
const (
	PermProfileReadOwn     Permission = "profile:read:own"
	PermWorldRead          Permission = "world:read"
	PermScenarioPlay       Permission = "scenario:play"
	PermRangeUseOwn        Permission = "range:use:own"
	PermEvidenceManageOwn  Permission = "evidence:manage:own"
	PermStudentRead        Permission = "student:read"
	PermScenarioAssign     Permission = "scenario:assign"
	PermProgressReview     Permission = "progress:review"
	PermScenarioCreate     Permission = "scenario:create"
	PermScenarioEdit       Permission = "scenario:edit"
	PermScenarioPublish    Permission = "scenario:publish"
	PermPlatformAdminister Permission = "platform:administer"
)

var rolePermissions = map[Role][]Permission{
	RolePlayer: {
		PermProfileReadOwn, PermWorldRead, PermScenarioPlay, PermRangeUseOwn, PermEvidenceManageOwn,
	},
	RoleInstructor: {
		PermProfileReadOwn, PermWorldRead, PermStudentRead, PermScenarioAssign, PermProgressReview,
	},
	RoleScenarioCreator: {
		PermProfileReadOwn, PermWorldRead, PermScenarioCreate, PermScenarioEdit, PermScenarioPublish,
	},
	RoleAdmin: {
		PermProfileReadOwn, PermWorldRead, PermStudentRead, PermScenarioAssign, PermProgressReview,
		PermScenarioCreate, PermScenarioEdit, PermScenarioPublish, PermPlatformAdminister,
	},
}

// RoleSet is an immutable set of recognised platform roles.
type RoleSet struct {
	roles []Role
}

// ParseRoles keeps only exact, known platform role names. Keycloak's
// built-in roles (offline_access, default-roles-*) and anything else are
// ignored, and matching is case-sensitive so "admin" never becomes ADMIN.
func ParseRoles(names []string) RoleSet {
	var out []Role
	for _, n := range names {
		r := Role(n)
		if slices.Contains(knownRoles, r) && !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return RoleSet{roles: out}
}

// Has reports whether the set contains r.
func (s RoleSet) Has(r Role) bool { return slices.Contains(s.roles, r) }

// Roles returns the roles in stable order.
func (s RoleSet) Roles() []Role { return slices.Clone(s.roles) }

// Can reports whether any role in the set grants p.
func (s RoleSet) Can(p Permission) bool {
	for _, r := range s.roles {
		if slices.Contains(rolePermissions[r], p) {
			return true
		}
	}
	return false
}

// Permissions returns the union of permissions granted by the set, sorted.
func (s RoleSet) Permissions() []Permission {
	var out []Permission
	for _, r := range s.roles {
		for _, p := range rolePermissions[r] {
			if !slices.Contains(out, p) {
				out = append(out, p)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

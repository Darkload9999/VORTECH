package auth_test

import (
	"slices"
	"testing"

	"github.com/Darkload9999/VORTECH/backend/internal/auth"
)

func TestParseRoles(t *testing.T) {
	rs := auth.ParseRoles([]string{"offline_access", "PLAYER", "default-roles-vortech", "PLAYER", "admin", "INSTRUCTOR", ""})
	got := rs.Roles()
	want := []auth.Role{auth.RoleInstructor, auth.RolePlayer}
	if !slices.Equal(got, want) {
		t.Fatalf("Roles() = %v, want %v", got, want)
	}
	if rs.Has(auth.RoleAdmin) {
		t.Fatal("lowercase admin must not map to ADMIN")
	}
}

func TestRolePermissionMatrix(t *testing.T) {
	tests := []struct {
		role  auth.Role
		allow []auth.Permission
		deny  []auth.Permission
	}{
		{
			auth.RolePlayer,
			[]auth.Permission{auth.PermProfileReadOwn, auth.PermScenarioPlay, auth.PermRangeUseOwn, auth.PermEvidenceManageOwn},
			[]auth.Permission{auth.PermStudentRead, auth.PermScenarioAssign, auth.PermScenarioCreate, auth.PermScenarioPublish, auth.PermPlatformAdminister},
		},
		{
			auth.RoleInstructor,
			[]auth.Permission{auth.PermProfileReadOwn, auth.PermStudentRead, auth.PermScenarioAssign, auth.PermProgressReview},
			[]auth.Permission{auth.PermScenarioCreate, auth.PermScenarioPublish, auth.PermPlatformAdminister, auth.PermRangeUseOwn},
		},
		{
			auth.RoleScenarioCreator,
			[]auth.Permission{auth.PermScenarioCreate, auth.PermScenarioEdit, auth.PermScenarioPublish},
			[]auth.Permission{auth.PermStudentRead, auth.PermPlatformAdminister, auth.PermRangeUseOwn},
		},
		{
			auth.RoleAdmin,
			[]auth.Permission{auth.PermPlatformAdminister, auth.PermScenarioPublish, auth.PermStudentRead},
			// Admins administer the platform; they do not get players' own
			// ranges or evidence by role. Support access is a separate,
			// audited feature.
			[]auth.Permission{auth.PermRangeUseOwn, auth.PermEvidenceManageOwn},
		},
	}
	for _, tt := range tests {
		t.Run(string(tt.role), func(t *testing.T) {
			rs := auth.ParseRoles([]string{string(tt.role)})
			for _, p := range tt.allow {
				if !rs.Can(p) {
					t.Errorf("%s should have %s", tt.role, p)
				}
			}
			for _, p := range tt.deny {
				if rs.Can(p) {
					t.Errorf("%s must not have %s", tt.role, p)
				}
			}
		})
	}
}

func TestEveryRoleCanReadTheWorld(t *testing.T) {
	for _, r := range []string{"PLAYER", "INSTRUCTOR", "SCENARIO_CREATOR", "ADMIN"} {
		if !auth.ParseRoles([]string{r}).Can(auth.PermWorldRead) {
			t.Errorf("%s must have world:read", r)
		}
	}
}

func TestNoRolesNoPermissions(t *testing.T) {
	rs := auth.ParseRoles([]string{"offline_access", "uma_authorization"})
	if len(rs.Permissions()) != 0 || rs.Can(auth.PermProfileReadOwn) {
		t.Fatalf("account without platform roles must have no permissions: %v", rs.Permissions())
	}
}

func TestPermissionsUnionIsSortedAndUnique(t *testing.T) {
	ps := auth.ParseRoles([]string{"PLAYER", "INSTRUCTOR"}).Permissions()
	if !slices.IsSorted(ps) {
		t.Fatalf("permissions not sorted: %v", ps)
	}
	if len(slices.Compact(slices.Clone(ps))) != len(ps) {
		t.Fatalf("duplicate permissions: %v", ps)
	}
}

package world

import (
	"testing"

	"github.com/google/uuid"
)

var (
	site     = uuid.MustParse("00000000-0000-7000-8000-000000000001")
	lobby    = uuid.MustParse("00000000-0000-7000-8000-000000000002")
	floor3   = uuid.MustParse("00000000-0000-7000-8000-000000000003")
	vault    = uuid.MustParse("00000000-0000-7000-8000-000000000004")
	inVault  = uuid.MustParse("00000000-0000-7000-8000-000000000005")
	branch   = uuid.MustParse("00000000-0000-7000-8000-000000000006")
	branchF  = uuid.MustParse("00000000-0000-7000-8000-000000000007")
	orphanID = uuid.MustParse("00000000-0000-7000-8000-000000000099")
)

func rules() []ZoneRule {
	return []ZoneRule{
		{ID: site, Code: "HQ", UnlockedByDefault: true},
		{ID: lobby, Code: "LOBBY", ParentID: &site, UnlockedByDefault: true},
		{ID: floor3, Code: "F3", ParentID: &site, RequiredCareerRank: 1},           // rank-gated
		{ID: vault, Code: "VAULT", ParentID: &site},                                // explicit-only
		{ID: inVault, Code: "IN_VAULT", ParentID: &vault, UnlockedByDefault: true}, // open, but parent locked
		{ID: branch, Code: "BRANCH", RequiredCareerRank: 2},                        // rank-gated site
		{ID: branchF, Code: "BRANCH_F", ParentID: &branch, RequiredCareerRank: 2},
	}
}

func TestAccessRules(t *testing.T) {
	tests := []struct {
		name     string
		rank     int32
		unlocked []uuid.UUID
		zone     uuid.UUID
		want     Decision
	}{
		{"default zone", 0, nil, lobby, Decision{Accessible: true}},
		{"rank-gated below rank", 0, nil, floor3, Decision{Reason: ReasonInsufficientRank, RequiredRank: 1}},
		{"rank-gated at rank", 1, nil, floor3, Decision{Accessible: true}},
		{"explicit-only without grant", 5, nil, vault, Decision{Reason: ReasonZoneLocked}},
		{"explicit-only with grant", 0, []uuid.UUID{vault}, vault, Decision{Accessible: true}},
		{"grant bypasses rank", 0, []uuid.UUID{floor3}, floor3, Decision{Accessible: true}},
		{"child of locked parent", 5, nil, inVault, Decision{Reason: ReasonParentLocked}},
		{"child once parent granted", 0, []uuid.UUID{vault}, inVault, Decision{Accessible: true}},
		{"rank-gated site and floor", 2, nil, branchF, Decision{Accessible: true}},
		{"rank-gated floor below rank", 1, nil, branchF, Decision{Reason: ReasonInsufficientRank, RequiredRank: 2}},
		{"granted child of rank-gated parent", 0, []uuid.UUID{branchF}, branchF, Decision{Reason: ReasonParentLocked}},
		{"unknown zone", 5, nil, orphanID, Decision{Reason: ReasonUnknownZone}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			unlocked := map[uuid.UUID]bool{}
			for _, id := range tt.unlocked {
				unlocked[id] = true
			}
			if got := NewAccess(rules(), unlocked, tt.rank).Decide(tt.zone); got != tt.want {
				t.Fatalf("Decide = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestAccessParentCycleIsDenied(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	zs := []ZoneRule{
		{ID: a, ParentID: &b, UnlockedByDefault: true},
		{ID: b, ParentID: &a, UnlockedByDefault: true},
	}
	if d := NewAccess(zs, nil, 0).Decide(a); d.Accessible {
		t.Fatal("a parent cycle must never grant access")
	}
}

func TestAccessAll(t *testing.T) {
	all := NewAccess(rules(), nil, 0).All()
	if len(all) != 7 || !all[lobby].Accessible || all[floor3].Accessible {
		t.Fatalf("unexpected decisions %+v", all)
	}
}

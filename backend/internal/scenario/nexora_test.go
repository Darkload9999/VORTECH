package scenario

import (
	"errors"
	"testing"
)

// nexoraDir is the shipped default scenario, relative to this package.
const nexoraDir = "../../../scenarios/nexora"

func TestNexoraScenarioIsValid(t *testing.T) {
	b, err := Load(nexoraDir)
	if err != nil {
		var ve *ValidationError
		if errors.As(err, &ve) {
			for _, p := range ve.Problems {
				t.Error(p.String())
			}
			t.FailNow()
		}
		t.Fatal(err)
	}
	if b.Scenario.Slug != "nexora" || b.Company.EmailDomain != "nexora.local" {
		t.Fatalf("unexpected scenario %+v / %+v", b.Scenario, b.Company)
	}
	if len(b.Departments) != 8 {
		t.Errorf("departments = %d, want the 8 NEXORA departments", len(b.Departments))
	}
	if len(b.Hash) != 64 {
		t.Errorf("content hash = %q", b.Hash)
	}

	// The spec's digital-twin example must hold.
	var pc *Asset
	for i := range b.Assets {
		if b.Assets[i].Code == "HQ-FIN-PC-04" {
			pc = &b.Assets[i]
		}
	}
	if pc == nil || pc.Hostname != "FIN-PC04" || pc.IP != "10.20.30.44" || pc.Owner != "EMP-0018" || pc.Network != "NET-HQ-FINANCE" {
		t.Fatalf("HQ-FIN-PC-04 does not match the documented example: %+v", pc)
	}
	var alex *Employee
	for i := range b.Employees {
		if b.Employees[i].Code == "EMP-0018" {
			alex = &b.Employees[i]
		}
	}
	if alex == nil || alex.Identity == nil || alex.Identity.Username != "alex" || alex.Department != "FIN" {
		t.Fatalf("EMP-0018 does not match the documented example: %+v", alex)
	}
	if len(b.RangeTemplates) != 2 {
		t.Fatalf("range templates = %d, want 2", len(b.RangeTemplates))
	}
	for _, rt := range b.RangeTemplates {
		if ws, ok := rt.Spec().TerminalWorkload(); !ok || ws.Name != "workstation" || len(ws.WritablePaths) != 2 {
			t.Errorf("%s: YAML anchor workstation not expanded: %+v", rt.Slug, ws)
		}
	}
	if len(alex.Schedule) != 3 {
		t.Errorf("YAML anchor schedule not expanded: %+v", alex.Schedule)
	}
}

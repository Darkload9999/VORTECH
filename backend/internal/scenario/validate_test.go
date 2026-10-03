package scenario

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mutated copies NEXORA into a temp dir and applies one textual edit.
func mutated(t *testing.T, file, old, replacement string) string {
	t.Helper()
	dir := t.TempDir()
	entries, err := os.ReadDir(nexoraDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(nexoraDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if e.Name() == file {
			s := string(b)
			if old != "" && !strings.Contains(s, old) {
				t.Fatalf("%s does not contain %q; the fixture drifted", file, old)
			}
			b = []byte(strings.Replace(s, old, replacement, 1))
		}
		if err := os.WriteFile(filepath.Join(dir, e.Name()), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func expectProblem(t *testing.T, dir, file, want string) {
	t.Helper()
	_, err := Load(dir)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected *ValidationError, got %v", err)
	}
	for _, p := range ve.Problems {
		if p.File == file && strings.Contains(p.String(), want) {
			return
		}
	}
	t.Fatalf("no %s problem containing %q in:\n%v", file, want, err)
}

func TestValidationFailures(t *testing.T) {
	tests := []struct {
		name, file, old, new, want string
	}{
		// Schema-level.
		{"real email domain", "company.yaml", "email_domain: nexora.local", "email_domain: nexora.com", "/email_domain"},
		{"unknown field", "company.yaml", "industry:", "ceo_salary: 1\nindustry:", "ceo_salary"},
		{"bad api version", "scenario.yaml", "apiVersion: vortech/v1", "apiVersion: vortech/v9", "/apiVersion"},
		{"bad object key", "world.yaml", "key: finance_pc_04,", "key: Finance PC,", "/objects"},
		{"invalid YAML", "network.yaml", "networks:", "networks: [unclosed", "invalid YAML"},
		{"two documents", "scenario.yaml", "kind: Scenario", "kind: Scenario\n---\nkind: Other", "exactly one YAML document"},

		{"range ttl too long", "ranges.yaml", "ttl_minutes: 60", "ttl_minutes: 100000", "/ttl_minutes"},
		{"range unknown field", "ranges.yaml", "cpu_millis: 50", "cpu_millis: 50\n        privileged: true", "privileged"},

		// Cross-reference.
		{"unknown department", "employees.yaml", "department: FIN\n    manager: EMP-0008\n    location: FINANCE_DEPT\n    persona: Helpful",
			"department: NOPE\n    manager: EMP-0008\n    location: FINANCE_DEPT\n    persona: Helpful", `unknown department "NOPE"`},
		{"manager cycle", "employees.yaml", "job_title: Chief Executive Officer\n    department: EXEC",
			"job_title: Chief Executive Officer\n    department: EXEC\n    manager: EMP-0003", "manager hierarchy contains a cycle"},
		{"duplicate asset code", "assets.yaml", "code: HQ-FIN-PC-05,", "code: HQ-FIN-PC-04,", `duplicate asset "HQ-FIN-PC-04"`},
		{"ip outside network", "assets.yaml", "ip: 10.20.30.44", "ip: 10.20.99.44", "outside network NET-HQ-FINANCE"},
		{"duplicate ip", "assets.yaml", "ip: 10.20.30.45", "ip: 10.20.30.44", "IP 10.20.30.44 already used"},
		{"host bits in cidr", "network.yaml", "cidr: 10.20.30.0/24", "cidr: 10.20.30.7/24", "host bits set"},
		{"door without target", "world.yaml", "leads_to: HQ_FLOOR_2, interactions: [OPEN_DOOR, CLOSE_DOOR] }",
			"interactions: [OPEN_DOOR, CLOSE_DOOR] }", "doors must declare the zone"},
		{"interaction not allowed", "world.yaml", "asset: HQ-FIN-PC-01, interactions: [USE_WORKSTATION, INSPECT_OBJECT]",
			"asset: HQ-FIN-PC-01, interactions: [OPEN_DOOR, INSPECT_OBJECT]", "OPEN_DOOR is not allowed on a workstation"},
		{"document without content", "world.yaml", "interactions: [READ_DOCUMENT]\n    content: |\n      Q3 CLOSE CHECKLIST",
			"interactions: [READ_DOCUMENT]\n    description: |\n      Q3 CLOSE CHECKLIST", "READ_DOCUMENT requires content"},
		{"unknown object asset", "world.yaml", "asset: HQ-FIN-PC-04,", "asset: HQ-FIN-PC-99,", `unknown asset "HQ-FIN-PC-99"`},
		{"group is not a group", "employees.yaml", "groups: [GRP-ALL-STAFF, GRP-FINANCE], access: [APP-LEDGER, APP-MAIL] }",
			"groups: [GRP-ALL-STAFF, APP-LEDGER], access: [APP-LEDGER, APP-MAIL] }", "not an identity_group"},
		{"overlapping schedule", "employees.yaml", "- { days: [weekdays], from: \"12:00\", to: \"13:00\", activity: break }",
			"- { days: [weekdays], from: \"11:00\", to: \"13:00\", activity: break }", "overlaps schedule"},
		{"unknown relationship node", "assets.yaml", `to: "asset:APP-LEDGER" }`, `to: "asset:APP-NOPE" }`, `unknown asset "APP-NOPE"`},
		{"head outside department", "departments.yaml", "head: EMP-0005", "head: EMP-0018", "does not belong to department HR"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expectProblem(t, mutated(t, tt.file, tt.old, tt.new), tt.file, tt.want)
		})
	}
}

func TestRangeTemplateSpecChecks(t *testing.T) {
	tests := []struct{ name, old, new, want string }{
		{"latest tag", "image: docker.io/library/alpine:3.20", "image: docker.io/library/alpine:latest", "latest"},
		{"unqualified image", "image: docker.io/traefik/whoami:v1.10.3", "image: whoami", "fully qualified"},
		{"rule to unknown workload", "{ from: app01, to: db01, port: 5432 }", "{ from: app01, to: db99, port: 5432 }", `unknown workload "db99"`},
		{"rule to closed port", "{ from: app01, to: db01, port: 5432 }", "{ from: app01, to: db01, port: 5433 }", "does not expose port 5433"},
		{"no terminal", "terminal: true", "terminal: false", "exactly one workload"},
		{"duplicate slug", "slug: nexora-corp-net", "slug: nexora-web-basics", `duplicate range template "nexora-web-basics"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expectProblem(t, mutated(t, "ranges.yaml", tt.old, tt.new), "ranges.yaml", tt.want)
		})
	}
}

func TestRangesFileIsOptional(t *testing.T) {
	dir := mutated(t, "", "", "")
	if err := os.Remove(filepath.Join(dir, "ranges.yaml")); err != nil {
		t.Fatal(err)
	}
	b, err := Load(dir)
	if err != nil || len(b.RangeTemplates) != 0 {
		t.Fatalf("scenario without ranges.yaml: %v", err)
	}
}

func TestNoEntryZone(t *testing.T) {
	dir := mutated(t, "world.yaml", "", "")
	p := filepath.Join(dir, "world.yaml")
	b, _ := os.ReadFile(p)
	s := strings.ReplaceAll(string(b), "unlocked_by_default: true", "unlocked_by_default: false")
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	expectProblem(t, dir, "world.yaml", "at least one zone must be unlocked by default")
}

func TestMissingFile(t *testing.T) {
	dir := mutated(t, "", "", "")
	if err := os.Remove(filepath.Join(dir, "network.yaml")); err != nil {
		t.Fatal(err)
	}
	expectProblem(t, dir, "network.yaml", "file is missing")
}

func TestYAMLAliasBombRejected(t *testing.T) {
	dir := mutated(t, "", "", "")
	bomb := "a: &a [x,x,x,x,x,x,x,x,x]\nb: &b [*a,*a,*a,*a,*a,*a,*a,*a,*a]\nc: &c [*b,*b,*b,*b,*b,*b,*b,*b,*b]\n" +
		"d: &d [*c,*c,*c,*c,*c,*c,*c,*c,*c]\ne: &e [*d,*d,*d,*d,*d,*d,*d,*d,*d]\nf: &f [*e,*e,*e,*e,*e,*e,*e,*e,*e]\n" +
		"g: &g [*f,*f,*f,*f,*f,*f,*f,*f,*f]\nh: &h [*g,*g,*g,*g,*g,*g,*g,*g,*g]\ni: [*h,*h,*h,*h,*h,*h,*h,*h,*h]\n"
	if err := os.WriteFile(filepath.Join(dir, "scenario.yaml"), []byte(bomb), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "excessive aliasing") {
		t.Fatalf("alias bomb must be rejected by the YAML decoder, got %v", err)
	}
}

func TestAllProblemsReportedTogether(t *testing.T) {
	dir := mutated(t, "assets.yaml", "ip: 10.20.30.44", "ip: 10.20.99.44")
	p := filepath.Join(dir, "assets.yaml")
	b, _ := os.ReadFile(p)
	s := strings.Replace(string(b), `to: "asset:APP-LEDGER" }`, `to: "asset:APP-NOPE" }`, 1)
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(dir)
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Problems) < 2 {
		t.Fatalf("expected every problem at once, got %v", err)
	}
}

func TestDaysOf(t *testing.T) {
	got := DaysOf([]string{"weekend", "mon", "weekdays"})
	want := []int32{1, 2, 3, 4, 5, 6, 7}
	if len(got) != len(want) {
		t.Fatalf("DaysOf = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("DaysOf = %v", got)
		}
	}
}

func TestContentHashIsStable(t *testing.T) {
	a, err := Load(nexoraDir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Load(mutated(t, "", "", ""))
	if err != nil {
		t.Fatal(err)
	}
	if a.Hash != b.Hash {
		t.Fatal("identical content must hash identically regardless of directory")
	}
	c, err := Load(mutated(t, "company.yaml", "founded_year: 2009", "founded_year: 2010"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Hash == a.Hash {
		t.Fatal("changed content must change the hash")
	}
}

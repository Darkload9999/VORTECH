package cyberrange

import (
	"strings"
	"testing"
)

func TestTransitions(t *testing.T) {
	valid := [][2]State{
		{Requested, Provisioning}, {Requested, Queued}, {Queued, Provisioning}, {Provisioning, Starting},
		{Starting, Ready}, {Ready, Active}, {Active, Stopping}, {Ready, Expired}, {Expired, Stopping},
		{Provisioning, Failed}, {Failed, Stopping}, {Stopping, Destroyed}, {Queued, Stopping},
	}
	for _, tr := range valid {
		if err := CheckTransition(tr[0], tr[1]); err != nil {
			t.Errorf("%s → %s should be valid: %v", tr[0], tr[1], err)
		}
	}
	invalid := [][2]State{
		{Destroyed, Requested}, {Destroyed, Ready}, {Requested, Ready}, {Ready, Provisioning},
		{Stopping, Ready}, {Failed, Ready}, {Expired, Active}, {Queued, Ready}, {Active, Ready},
	}
	for _, tr := range invalid {
		if CanTransition(tr[0], tr[1]) {
			t.Errorf("%s → %s must be invalid", tr[0], tr[1])
		}
	}
	// Every state reaches DESTROYED and nothing leaves it.
	for s := range transitions {
		if s != Destroyed && !reaches(s, Destroyed, map[State]bool{}) {
			t.Errorf("%s cannot reach DESTROYED (resources could leak)", s)
		}
	}
	if len(transitions[Destroyed]) != 0 {
		t.Error("DESTROYED must be terminal")
	}
}

func reaches(from, to State, seen map[State]bool) bool {
	if from == to {
		return true
	}
	if seen[from] {
		return false
	}
	seen[from] = true
	for _, n := range transitions[from] {
		if reaches(n, to, seen) {
			return true
		}
	}
	return false
}

func TestStatePredicates(t *testing.T) {
	if !Queued.Live() || Stopping.Live() || Destroyed.Live() {
		t.Error("Live() wrong")
	}
	if Queued.ConsumesCapacity() || Requested.ConsumesCapacity() || !Failed.ConsumesCapacity() || Destroyed.ConsumesCapacity() {
		t.Error("ConsumesCapacity() wrong")
	}
	if !Ready.Stoppable() || Destroyed.Stoppable() || Stopping.Stoppable() {
		t.Error("Stoppable() wrong")
	}
}

func TestCapacity(t *testing.T) {
	c := Capacity{
		Cluster:         Resources{CPUMillis: 8000, MemoryMiB: 32768, StorageMiB: 400000},
		Reserved:        Resources{CPUMillis: 2000, MemoryMiB: 8192, StorageMiB: 100000},
		HeadroomPercent: 10,
	}
	if a := c.Available(); a.CPUMillis != 5400 || a.MemoryMiB != 22118 || a.StorageMiB != 270000 {
		t.Fatalf("Available = %+v", a)
	}
	one := Resources{CPUMillis: 1200, MemoryMiB: 1200, StorageMiB: 1000}
	inUse := Resources{}
	n := 0
	for {
		ok, _ := c.Fits(inUse, one)
		if !ok {
			break
		}
		inUse = inUse.Add(one)
		n++
	}
	if n != 4 {
		t.Fatalf("expected 4 ranges to fit on the 8 vCPU VPS, got %d", n)
	}
	if ok, dim := c.Fits(inUse, one); ok || dim != "cpu" {
		t.Fatalf("blocking dimension = %q", dim)
	}
	if ok, dim := c.Fits(Resources{}, Resources{CPUMillis: 1, MemoryMiB: 999999}); ok || dim != "memory" {
		t.Fatalf("memory should block: %q", dim)
	}
	over := Capacity{Cluster: Resources{CPUMillis: 1000}, Reserved: Resources{CPUMillis: 2000}}
	if over.Available().CPUMillis != 0 {
		t.Fatal("reserve above cluster size must yield zero, not negative capacity")
	}
}

func validSpec() Spec {
	return Spec{
		Workloads: []Workload{
			{Name: "workstation", Role: "workstation", Image: "docker.io/library/alpine:3.20", CPUMillis: 500, MemoryMiB: 256, StorageMiB: 64,
				RunAsUser: 1000, ReadOnlyRoot: true, WritablePaths: []string{"/tmp"}, Terminal: true},
			{Name: "web01", Role: "web", Image: "docker.io/nginxinc/nginx-unprivileged:1.27-alpine", CPUMillis: 100, MemoryMiB: 64, StorageMiB: 32,
				RunAsUser: 101, Ports: []Port{{Name: "http", Port: 8080}}, WritablePaths: []string{"/tmp"},
				Env: map[string]string{"ADMIN_PASSWORD": "secret:web_admin"}},
		},
		Network: []NetworkRule{{From: "workstation", To: "web01", Port: 8080}},
	}
}

func TestSpecValidate(t *testing.T) {
	if errs := validSpec().Validate([]string{"docker.io/library/", "docker.io/nginxinc/"}); len(errs) != 0 {
		t.Fatalf("valid spec rejected: %v", errs)
	}
	tests := []struct {
		name   string
		mutate func(*Spec)
		want   string
	}{
		{"no terminal", func(s *Spec) { s.Workloads[0].Terminal = false }, "exactly one workload"},
		{"two terminals", func(s *Spec) { s.Workloads[1].Terminal = true }, "exactly one workload"},
		{"root user", func(s *Spec) { s.Workloads[0].RunAsUser = 0 }, "non-root"},
		{"privileged port", func(s *Spec) { s.Workloads[1].Ports[0].Port = 80 }, "1024–65535"},
		{"latest tag", func(s *Spec) { s.Workloads[0].Image = "docker.io/library/alpine:latest" }, "latest"},
		{"untagged image", func(s *Spec) { s.Workloads[0].Image = "alpine" }, "fully qualified"},
		{"disallowed image", func(s *Spec) { s.Workloads[0].Image = "evil.example/miner:1.0" }, "not in the allowed"},
		{"bad name", func(s *Spec) { s.Workloads[1].Name = "Web_01" }, "DNS label"},
		{"duplicate name", func(s *Spec) { s.Workloads[1].Name = "workstation" }, "duplicate"},
		{"rule to unknown", func(s *Spec) { s.Network[0].To = "db99" }, "unknown workload"},
		{"rule to closed port", func(s *Spec) { s.Network[0].Port = 9999 }, "does not expose port"},
		{"path traversal", func(s *Spec) { s.Workloads[0].WritablePaths = []string{"/tmp/../etc"} }, "absolute path"},
		{"writable without storage", func(s *Spec) { s.Workloads[0].StorageMiB = 0 }, "need storage_mib"},
		{"huge cpu", func(s *Spec) { s.Workloads[0].CPUMillis = 64000 }, "cpu_millis"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := validSpec()
			s.Workloads = append([]Workload(nil), s.Workloads...)
			s.Workloads[1].Ports = append([]Port(nil), s.Workloads[1].Ports...)
			s.Network = append([]NetworkRule(nil), s.Network...)
			tt.mutate(&s)
			errs := s.Validate([]string{"docker.io/library/", "docker.io/nginxinc/"})
			if !strings.Contains(strings.Join(errs, "; "), tt.want) {
				t.Fatalf("errors %v do not mention %q", errs, tt.want)
			}
		})
	}
}

func TestSpecHelpers(t *testing.T) {
	s := validSpec()
	cpu, mem, st := s.Totals()
	if cpu != 600 || mem != 320 || st != 96 {
		t.Fatalf("Totals = %d %d %d", cpu, mem, st)
	}
	if w, ok := s.TerminalWorkload(); !ok || w.Name != "workstation" {
		t.Fatal("TerminalWorkload")
	}
	if k := s.SecretKeys(); len(k) != 1 || k[0] != "web_admin" {
		t.Fatalf("SecretKeys = %v", k)
	}
}

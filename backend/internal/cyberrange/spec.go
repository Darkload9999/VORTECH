package cyberrange

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/google/uuid"
)

// Spec describes the workloads and network of a range template. It is
// authored in scenarios/<slug>/ranges.yaml, validated by the scenario
// importer and stored as JSON on range_templates.spec.
type Spec struct {
	Workloads []Workload    `json:"workloads"`
	Network   []NetworkRule `json:"network"`
}

// Workload is one container in a range, reachable inside the range by its
// name (it becomes the Service and hostname: web01, db01, ...).
type Workload struct {
	Name          string            `json:"name"`
	Role          string            `json:"role"`
	Image         string            `json:"image"`
	Command       []string          `json:"command,omitempty"`
	Args          []string          `json:"args,omitempty"`
	Ports         []Port            `json:"ports,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
	CPUMillis     int32             `json:"cpu_millis"`
	MemoryMiB     int32             `json:"memory_mib"`
	StorageMiB    int32             `json:"storage_mib"`
	RunAsUser     int64             `json:"run_as_user"`
	ReadOnlyRoot  bool              `json:"read_only_root"`
	WritablePaths []string          `json:"writable_paths,omitempty"`
	// Terminal marks the workload players get a shell in (exactly one).
	Terminal bool `json:"terminal,omitempty"`
}

// Port is a TCP port a workload listens on.
type Port struct {
	Name string `json:"name"`
	Port int32  `json:"port"`
}

// NetworkRule allows traffic from one workload to another. Port 0 allows
// all of the target's declared ports.
type NetworkRule struct {
	From string `json:"from"`
	To   string `json:"to"`
	Port int32  `json:"port,omitempty"`
}

// SecretPrefix marks an env value generated per range and stored only in
// a Kubernetes Secret, e.g. DB_PASSWORD: "secret:db_password".
const SecretPrefix = "secret:"

var (
	dnsLabel   = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,30}[a-z0-9])?$`)
	envName    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	secretKey  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	imageRef   = regexp.MustCompile(`^[a-z0-9.-]+(:[0-9]+)?/[a-z0-9._/-]+(:[A-Za-z0-9._-]+|@sha256:[a-f0-9]{64})$`)
	absPath    = regexp.MustCompile(`^/[A-Za-z0-9._/-]{1,200}$`)
	validRoles = []string{"workstation", "web", "app", "database", "git", "bastion", "service"}
)

// Validate checks a spec's internal consistency. allowedImages, if
// non-empty, restricts images to those prefixes.
func (s Spec) Validate(allowedImages []string) []string {
	var errs []string
	add := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }

	if len(s.Workloads) == 0 || len(s.Workloads) > 20 {
		add("a range needs 1–20 workloads")
	}
	names := map[string]Workload{}
	terminals := 0
	for i, w := range s.Workloads {
		p := fmt.Sprintf("workloads[%d] (%s)", i, w.Name)
		if !dnsLabel.MatchString(w.Name) {
			add("%s: name must be a DNS label (lowercase letters, digits, '-')", p)
		}
		if _, dup := names[w.Name]; dup {
			add("%s: duplicate workload name", p)
		}
		names[w.Name] = w
		if !slices.Contains(validRoles, w.Role) {
			add("%s: role must be one of %s", p, strings.Join(validRoles, ", "))
		}
		if !imageRef.MatchString(w.Image) {
			add("%s: image must be a fully qualified reference with a tag or digest (registry/repo:tag)", p)
		} else if strings.HasSuffix(w.Image, ":latest") {
			add("%s: the 'latest' tag is not allowed; pin a version", p)
		}
		if len(allowedImages) > 0 && !imageAllowed(w.Image, allowedImages) {
			add("%s: image %q is not in the allowed image list", p, w.Image)
		}
		if w.CPUMillis < 10 || w.CPUMillis > 4000 {
			add("%s: cpu_millis must be 10–4000", p)
		}
		if w.MemoryMiB < 16 || w.MemoryMiB > 8192 {
			add("%s: memory_mib must be 16–8192", p)
		}
		if w.StorageMiB < 0 || w.StorageMiB > 10240 {
			add("%s: storage_mib must be 0–10240", p)
		}
		if w.RunAsUser < 1 || w.RunAsUser > 65535 {
			add("%s: run_as_user must be a non-root UID (1–65535)", p)
		}
		for _, wp := range w.WritablePaths {
			if !absPath.MatchString(wp) || strings.Contains(wp, "..") {
				add("%s: writable path %q must be an absolute path", p, wp)
			}
		}
		if len(w.WritablePaths) > 0 && w.StorageMiB == 0 {
			add("%s: writable_paths need storage_mib > 0", p)
		}
		ports := map[int32]bool{}
		for _, pt := range w.Ports {
			if pt.Port < 1024 || pt.Port > 65535 {
				add("%s: port %d must be 1024–65535 (containers run without privileges)", p, pt.Port)
			}
			if !dnsLabel.MatchString(pt.Name) || len(pt.Name) > 15 {
				add("%s: port name %q must be a short DNS label", p, pt.Name)
			}
			if ports[pt.Port] {
				add("%s: duplicate port %d", p, pt.Port)
			}
			ports[pt.Port] = true
		}
		for k, v := range w.Env {
			if !envName.MatchString(k) {
				add("%s: env name %q is invalid", p, k)
			}
			if key, ok := strings.CutPrefix(v, SecretPrefix); ok && !secretKey.MatchString(key) {
				add("%s: secret key %q is invalid", p, key)
			}
		}
		if w.Terminal {
			terminals++
		}
	}
	if terminals != 1 {
		add("exactly one workload must have terminal: true (got %d)", terminals)
	}
	for i, r := range s.Network {
		p := fmt.Sprintf("network[%d]", i)
		_, okFrom := names[r.From]
		to, okTo := names[r.To]
		switch {
		case !okFrom:
			add("%s: unknown workload %q", p, r.From)
		case !okTo:
			add("%s: unknown workload %q", p, r.To)
		case r.From == r.To:
			add("%s: a workload always reaches itself", p)
		case len(to.Ports) == 0:
			add("%s: %s exposes no ports", p, to.Name)
		case r.Port != 0 && !slices.ContainsFunc(to.Ports, func(pt Port) bool { return pt.Port == r.Port }):
			add("%s: %s does not expose port %d", p, to.Name, r.Port)
		}
	}
	return errs
}

func imageAllowed(image string, allowed []string) bool {
	for _, a := range allowed {
		if strings.HasPrefix(image, a) {
			return true
		}
	}
	return false
}

// Totals sums the resources of all workloads. Limits are reserved in full
// (requests = limits), so capacity accounting never overcommits.
func (s Spec) Totals() (cpuMillis, memoryMiB, storageMiB int32) {
	for _, w := range s.Workloads {
		cpuMillis += w.CPUMillis
		memoryMiB += w.MemoryMiB
		storageMiB += w.StorageMiB
	}
	return
}

// TerminalWorkload returns the workload players get a shell in.
func (s Spec) TerminalWorkload() (Workload, bool) {
	for _, w := range s.Workloads {
		if w.Terminal {
			return w, true
		}
	}
	return Workload{}, false
}

// SecretKeys lists the generated secret keys the spec references.
func (s Spec) SecretKeys() []string {
	var keys []string
	for _, w := range s.Workloads {
		for _, v := range w.Env {
			if k, ok := strings.CutPrefix(v, SecretPrefix); ok && !slices.Contains(keys, k) {
				keys = append(keys, k)
			}
		}
	}
	slices.Sort(keys)
	return keys
}

// NamespaceFor returns the Kubernetes namespace of a range (the database
// enforces the same naming with a CHECK constraint).
func NamespaceFor(id uuid.UUID) string { return "range-" + id.String() }

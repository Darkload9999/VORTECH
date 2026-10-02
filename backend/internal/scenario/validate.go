package scenario

import (
	"fmt"
	"net/netip"
	"slices"
	"sort"
	"strings"
)

// Weekdays in ISO 8601 numbering (Monday = 1).
var dayNumbers = map[string][]int32{
	"mon": {1}, "tue": {2}, "wed": {3}, "thu": {4}, "fri": {5}, "sat": {6}, "sun": {7},
	"weekdays": {1, 2, 3, 4, 5},
	"weekend":  {6, 7},
}

// allowedInteractions lists, per object kind, the interactions a scenario
// may enable. The game server enforces the same table at runtime.
var allowedInteractions = map[string][]string{
	"door":         {"OPEN_DOOR", "CLOSE_DOOR", "INSPECT_OBJECT"},
	"workstation":  {"USE_WORKSTATION", "ACCESS_TERMINAL", "INSPECT_OBJECT", "START_ENGAGEMENT"},
	"terminal":     {"ACCESS_TERMINAL", "USE_WORKSTATION", "INSPECT_OBJECT", "START_ENGAGEMENT"},
	"screen":       {"READ_DOCUMENT", "INSPECT_OBJECT", "START_ENGAGEMENT"},
	"document":     {"READ_DOCUMENT", "INSPECT_OBJECT"},
	"whiteboard":   {"READ_DOCUMENT", "INSPECT_OBJECT"},
	"server_rack":  {"INSPECT_OBJECT"},
	"badge_reader": {"INSPECT_OBJECT"},
	"printer":      {"INSPECT_OBJECT"},
	"npc_spawn":    {"INSPECT_OBJECT"},
	"furniture":    {"INSPECT_OBJECT"},
}

// AllowedInteractions reports whether kind may offer interaction.
func AllowedInteractions(kind, interaction string) bool {
	return slices.Contains(allowedInteractions[kind], interaction)
}

// DaysOf expands schedule day names into ISO weekday numbers.
func DaysOf(days []string) []int32 {
	var out []int32
	for _, d := range days {
		for _, n := range dayNumbers[d] {
			if !slices.Contains(out, n) {
				out = append(out, n)
			}
		}
	}
	slices.Sort(out)
	return out
}

// applyDefaults fills optional fields so the importer and validator see
// explicit values.
func applyDefaults(b *Bundle) {
	for i := range b.Employees {
		e := &b.Employees[i]
		if e.DisplayName == "" {
			e.DisplayName = e.FirstName + " " + e.LastName
		}
		if e.EmploymentType == "" {
			e.EmploymentType = "full_time"
		}
		if e.Status == "" {
			e.Status = "active"
		}
		if e.NPC == nil {
			t := true
			e.NPC = &t
		}
		if id := e.Identity; id != nil {
			if id.Type == "" {
				id.Type = "user"
				if e.EmploymentType == "contractor" {
					id.Type = "contractor"
				}
			}
			if id.Status == "" {
				id.Status = "enabled"
			}
		}
	}
	for i := range b.Services {
		if b.Services[i].Status == "" {
			b.Services[i].Status = "enabled"
		}
	}
	for i := range b.Networks {
		if b.Networks[i].Criticality == "" {
			b.Networks[i].Criticality = "high"
		}
	}
	for i := range b.Assets {
		a := &b.Assets[i]
		if a.Criticality == "" {
			a.Criticality = "medium"
		}
		if a.Status == "" {
			a.Status = "active"
		}
	}
}

type checker struct {
	problems []Problem
}

func (c *checker) add(file, path, format string, args ...any) {
	c.problems = append(c.problems, Problem{File: file, Path: path, Message: fmt.Sprintf(format, args...)})
}

// index records codes for one namespace, reporting duplicates.
func (c *checker) index(file, kind string, codes []string, fold bool) map[string]int {
	m := make(map[string]int, len(codes))
	for i, code := range codes {
		k := code
		if fold {
			k = strings.ToLower(k)
		}
		if j, dup := m[k]; dup {
			c.add(file, fmt.Sprintf("%s[%d]", kind, i), "duplicate %s %q (first defined at index %d)", kind, code, j)
			continue
		}
		m[k] = i
	}
	return m
}

// cycles reports codes whose parent chain loops.
func (c *checker) cycles(file, kind string, parent map[string]string) {
	reported := map[string]bool{}
	keys := make([]string, 0, len(parent))
	for k := range parent {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, start := range keys {
		seen := map[string]bool{}
		for cur := start; cur != ""; cur = parent[cur] {
			if seen[cur] {
				if !reported[cur] {
					c.add(file, kind, "%s hierarchy contains a cycle through %q", kind, cur)
					reported[cur] = true
				}
				break
			}
			seen[cur] = true
		}
	}
}

func validateReferences(b *Bundle) []Problem {
	applyDefaults(b)
	c := &checker{}

	depts := c.index("departments.yaml", "department", mapCodes(b.Departments, func(d Department) string { return d.Code }), false)
	emps := c.index("employees.yaml", "employee", mapCodes(b.Employees, func(e Employee) string { return e.Code }), false)
	zones := c.index("world.yaml", "zone", mapCodes(b.World.Zones, func(z Zone) string { return z.Code }), false)
	locs := c.index("world.yaml", "location", mapCodes(b.World.Locations, func(l Location) string { return l.Code }), false)
	c.index("world.yaml", "object", mapCodes(b.World.Objects, func(o Object) string { return o.Key }), false)

	// Networks and assets share one code namespace (networks are assets).
	assetType := map[string]string{}
	assetCodes := make([]string, 0, len(b.Networks)+len(b.Assets))
	for _, n := range b.Networks {
		assetCodes = append(assetCodes, n.Code)
		assetType[n.Code] = "network"
	}
	for _, a := range b.Assets {
		assetCodes = append(assetCodes, a.Code)
		if _, ok := assetType[a.Code]; !ok {
			assetType[a.Code] = a.Type
		}
	}
	c.index("assets.yaml", "asset", assetCodes, false)

	var usernames []string
	for _, e := range b.Employees {
		if e.Identity != nil {
			usernames = append(usernames, e.Identity.Username)
		}
	}
	for _, s := range b.Services {
		usernames = append(usernames, s.Username)
	}
	users := c.index("employees.yaml", "username", usernames, true)

	has := func(m map[string]int, k string) bool { _, ok := m[k]; return ok }

	// Departments.
	deptParent := map[string]string{}
	for i, d := range b.Departments {
		p := fmt.Sprintf("departments[%d]", i)
		if d.Parent != "" && !has(depts, d.Parent) {
			c.add("departments.yaml", p+".parent", "unknown department %q", d.Parent)
		}
		deptParent[d.Code] = d.Parent
		if d.Head != "" {
			if !has(emps, d.Head) {
				c.add("departments.yaml", p+".head", "unknown employee %q", d.Head)
			} else if b.Employees[emps[d.Head]].Department != d.Code {
				c.add("departments.yaml", p+".head", "head %q does not belong to department %s", d.Head, d.Code)
			}
		}
	}
	c.cycles("departments.yaml", "department", deptParent)

	// World.
	zoneParent := map[string]string{}
	anyEntry := false
	for i, z := range b.World.Zones {
		if z.Parent != "" && !has(zones, z.Parent) {
			c.add("world.yaml", fmt.Sprintf("zones[%d].parent", i), "unknown zone %q", z.Parent)
		}
		zoneParent[z.Code] = z.Parent
		if z.UnlockedByDefault && z.RequiredCareerRank == 0 {
			anyEntry = true
		}
	}
	c.cycles("world.yaml", "zone", zoneParent)
	if !anyEntry {
		c.add("world.yaml", "zones", "at least one zone must be unlocked by default with required_career_rank 0, or new players cannot enter the world")
	}
	for i, l := range b.World.Locations {
		p := fmt.Sprintf("locations[%d]", i)
		if !has(zones, l.Zone) {
			c.add("world.yaml", p+".zone", "unknown zone %q", l.Zone)
		}
		if l.Department != "" && !has(depts, l.Department) {
			c.add("world.yaml", p+".department", "unknown department %q", l.Department)
		}
	}
	for i, o := range b.World.Objects {
		p := fmt.Sprintf("objects[%d]", i)
		if !has(locs, o.Location) {
			c.add("world.yaml", p+".location", "unknown location %q", o.Location)
		}
		if o.Asset != "" && assetType[o.Asset] == "" {
			c.add("world.yaml", p+".asset", "unknown asset %q", o.Asset)
		}
		switch {
		case o.Kind == "door" && o.LeadsTo == "":
			c.add("world.yaml", p+".leads_to", "doors must declare the zone they lead to")
		case o.Kind != "door" && o.LeadsTo != "":
			c.add("world.yaml", p+".leads_to", "only doors may lead to a zone")
		case o.LeadsTo != "" && !has(zones, o.LeadsTo):
			c.add("world.yaml", p+".leads_to", "unknown zone %q", o.LeadsTo)
		}
		for _, in := range o.Interactions {
			if !AllowedInteractions(o.Kind, in) {
				c.add("world.yaml", p+".interactions", "%s is not allowed on a %s", in, o.Kind)
			}
		}
		if slices.Contains(o.Interactions, "READ_DOCUMENT") && strings.TrimSpace(o.Content) == "" {
			c.add("world.yaml", p+".content", "READ_DOCUMENT requires content")
		}
	}

	// Employees.
	managers := map[string]string{}
	for i, e := range b.Employees {
		p := fmt.Sprintf("employees[%d]", i)
		if !has(depts, e.Department) {
			c.add("employees.yaml", p+".department", "unknown department %q", e.Department)
		}
		if e.Manager != "" && !has(emps, e.Manager) {
			c.add("employees.yaml", p+".manager", "unknown employee %q", e.Manager)
		}
		managers[e.Code] = e.Manager
		if e.Location != "" && !has(locs, e.Location) {
			c.add("employees.yaml", p+".location", "unknown location %q", e.Location)
		}
		if id := e.Identity; id != nil {
			c.checkMemberships("employees.yaml", p+".identity", id.Groups, id.Access, assetType)
		}
		c.checkSchedule(p, e.Schedule, locs)
	}
	c.cycles("employees.yaml", "manager", managers)
	for i, s := range b.Services {
		c.checkMemberships("employees.yaml", fmt.Sprintf("service_accounts[%d]", i), s.Groups, s.Access, assetType)
	}

	// Networks and assets.
	prefixes := map[string]netip.Prefix{}
	for i, n := range b.Networks {
		p := fmt.Sprintf("networks[%d]", i)
		pfx, err := netip.ParsePrefix(n.CIDR)
		if err != nil {
			c.add("network.yaml", p+".cidr", "invalid CIDR %q", n.CIDR)
		} else if pfx.Masked() != pfx {
			c.add("network.yaml", p+".cidr", "CIDR %q has host bits set (use %s)", n.CIDR, pfx.Masked())
		} else {
			prefixes[n.Code] = pfx
		}
		if n.Location != "" && !has(locs, n.Location) {
			c.add("network.yaml", p+".location", "unknown location %q", n.Location)
		}
	}
	hostnames := map[string]string{}
	ips := map[netip.Addr]string{}
	for i, a := range b.Assets {
		p := fmt.Sprintf("assets[%d]", i)
		if a.Department != "" && !has(depts, a.Department) {
			c.add("assets.yaml", p+".department", "unknown department %q", a.Department)
		}
		if a.Owner != "" && !has(emps, a.Owner) {
			c.add("assets.yaml", p+".owner", "unknown employee %q", a.Owner)
		}
		if a.Location != "" && !has(locs, a.Location) {
			c.add("assets.yaml", p+".location", "unknown location %q", a.Location)
		}
		if a.HostedOn != "" {
			switch assetType[a.HostedOn] {
			case "server", "virtual_machine", "cloud_resource", "storage":
			case "":
				c.add("assets.yaml", p+".hosted_on", "unknown asset %q", a.HostedOn)
			default:
				c.add("assets.yaml", p+".hosted_on", "%q (%s) cannot host other assets", a.HostedOn, assetType[a.HostedOn])
			}
		}
		for _, d := range a.DependsOn {
			if assetType[d] == "" {
				c.add("assets.yaml", p+".depends_on", "unknown asset %q", d)
			}
		}
		if a.Hostname != "" {
			k := strings.ToLower(a.Hostname)
			if prev, dup := hostnames[k]; dup {
				c.add("assets.yaml", p+".hostname", "hostname %q already used by %s", a.Hostname, prev)
			}
			hostnames[k] = a.Code
		}
		if a.IP != "" {
			ip, err := netip.ParseAddr(a.IP)
			if err != nil {
				c.add("assets.yaml", p+".ip", "invalid IP address %q", a.IP)
				continue
			}
			if prev, dup := ips[ip]; dup {
				c.add("assets.yaml", p+".ip", "IP %s already used by %s", ip, prev)
			}
			ips[ip] = a.Code
			if a.Network != "" {
				if pfx, ok := prefixes[a.Network]; ok && !pfx.Contains(ip) {
					c.add("assets.yaml", p+".ip", "IP %s is outside network %s (%s)", ip, a.Network, pfx)
				}
			}
		}
		if a.Network != "" && assetType[a.Network] != "network" {
			c.add("assets.yaml", p+".network", "%q is not a network from network.yaml", a.Network)
		}
	}

	// Explicit relationships.
	for i, r := range b.Relations {
		p := fmt.Sprintf("relationships[%d]", i)
		for side, ref := range map[string]string{"from": r.From, "to": r.To} {
			kind, code, _ := strings.Cut(ref, ":")
			ok := false
			switch kind {
			case "employee":
				ok = has(emps, code)
			case "identity":
				ok = has(users, strings.ToLower(code))
			case "asset":
				ok = assetType[code] != ""
			}
			if !ok {
				c.add("assets.yaml", p+"."+side, "unknown %s %q", kind, code)
			}
		}
		if r.From == r.To {
			c.add("assets.yaml", p, "relationship must connect two different nodes")
		}
	}

	sort.SliceStable(c.problems, func(i, j int) bool {
		if c.problems[i].File != c.problems[j].File {
			return c.problems[i].File < c.problems[j].File
		}
		return c.problems[i].Path < c.problems[j].Path
	})
	return c.problems
}

func (c *checker) checkMemberships(file, path string, groups, access []string, assetType map[string]string) {
	for _, g := range groups {
		switch assetType[g] {
		case "identity_group":
		case "":
			c.add(file, path+".groups", "unknown group %q", g)
		default:
			c.add(file, path+".groups", "%q is a %s, not an identity_group", g, assetType[g])
		}
	}
	for _, a := range access {
		switch assetType[a] {
		case "":
			c.add(file, path+".access", "unknown asset %q", a)
		case "identity_group", "network":
			c.add(file, path+".access", "%q is a %s; use groups for memberships", a, assetType[a])
		}
	}
}

func (c *checker) checkSchedule(path string, entries []ScheduleEntry, locs map[string]int) {
	type window struct {
		from, to string
		idx      int
	}
	byDay := map[int32][]window{}
	for i, s := range entries {
		p := fmt.Sprintf("%s.schedule[%d]", path, i)
		if s.From >= s.To {
			c.add("employees.yaml", p, "from (%s) must be before to (%s)", s.From, s.To)
			continue
		}
		if s.Location != "" {
			if _, ok := locs[s.Location]; !ok {
				c.add("employees.yaml", p+".location", "unknown location %q", s.Location)
			}
		}
		for _, d := range DaysOf(s.Days) {
			for _, w := range byDay[d] {
				if s.From < w.to && w.from < s.To {
					c.add("employees.yaml", p, "overlaps schedule[%d] on weekday %d", w.idx, d)
				}
			}
			byDay[d] = append(byDay[d], window{s.From, s.To, i})
		}
	}
}

func mapCodes[T any](items []T, f func(T) string) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = f(it)
	}
	return out
}

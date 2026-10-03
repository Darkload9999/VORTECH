package scenario

import (
	"time"

	"github.com/Darkload9999/VORTECH/backend/internal/cyberrange"
)

// Bundle is a fully loaded and validated scenario directory. The per-file
// types carry JSON tags matching the YAML (snake_case) because files are
// decoded through JSON after schema validation.
type Bundle struct {
	Dir  string
	Hash string // SHA-256 over the source files

	Scenario    Meta
	Company     Company
	Departments []Department
	Employees   []Employee
	Services    []ServiceAccount
	Networks    []Network
	Assets      []Asset
	Relations   []Relationship
	World       World
	// RangeTemplates come from the optional ranges.yaml.
	RangeTemplates []RangeTemplate
}

// Meta is scenario.yaml.
type Meta struct {
	APIVersion  string `json:"apiVersion"`
	Kind        string `json:"kind"`
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
}

// Company is company.yaml.
type Company struct {
	Code         string `json:"code"`
	Name         string `json:"name"`
	LegalName    string `json:"legal_name"`
	Industry     string `json:"industry"`
	Description  string `json:"description"`
	Headquarters string `json:"headquarters"`
	FoundedYear  *int32 `json:"founded_year"`
	EmailDomain  string `json:"email_domain"`
}

// Department is one entry of departments.yaml.
type Department struct {
	Code        string `json:"code"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Parent      string `json:"parent"`
	Head        string `json:"head"` // employee code
}

// Employee is one entry of employees.yaml.
type Employee struct {
	Code           string          `json:"code"`
	FirstName      string          `json:"first_name"`
	LastName       string          `json:"last_name"`
	DisplayName    string          `json:"display_name"`
	JobTitle       string          `json:"job_title"`
	Department     string          `json:"department"`
	Manager        string          `json:"manager"`
	Location       string          `json:"location"`
	EmploymentType string          `json:"employment_type"`
	Status         string          `json:"status"`
	NPC            *bool           `json:"npc"`
	Persona        string          `json:"persona"`
	Greeting       string          `json:"greeting"`
	Identity       *Identity       `json:"identity"`
	Schedule       []ScheduleEntry `json:"schedule"`
}

// Identity is an employee's fictional directory account.
type Identity struct {
	Username   string     `json:"username"`
	Type       string     `json:"type"`
	Status     string     `json:"status"`
	MFA        bool       `json:"mfa"`
	Privileged bool       `json:"privileged"`
	ExpiresAt  *time.Time `json:"expires_at"`
	Groups     []string   `json:"groups"` // identity_group asset codes (MEMBER_OF)
	Access     []string   `json:"access"` // asset codes (HAS_ACCESS_TO)
}

// ServiceAccount is a non-human directory identity.
type ServiceAccount struct {
	Username   string   `json:"username"`
	Status     string   `json:"status"`
	Privileged bool     `json:"privileged"`
	Groups     []string `json:"groups"`
	Access     []string `json:"access"`
}

// ScheduleEntry is a recurring block in an employee's week.
type ScheduleEntry struct {
	Days     []string `json:"days"`
	From     string   `json:"from"`
	To       string   `json:"to"`
	Location string   `json:"location"`
	Activity string   `json:"activity"`
}

// Network is one entry of network.yaml; imported as an asset of type network.
type Network struct {
	Code        string `json:"code"`
	Name        string `json:"name"`
	CIDR        string `json:"cidr"`
	VLAN        *int   `json:"vlan"`
	Description string `json:"description"`
	Criticality string `json:"criticality"`
	Location    string `json:"location"`
}

// Asset is one entry of assets.yaml.
type Asset struct {
	Code        string         `json:"code"`
	Name        string         `json:"name"`
	Type        string         `json:"type"`
	Hostname    string         `json:"hostname"`
	IP          string         `json:"ip"`
	OS          string         `json:"os"`
	Criticality string         `json:"criticality"`
	Status      string         `json:"status"`
	Department  string         `json:"department"`
	Owner       string         `json:"owner"`
	Location    string         `json:"location"`
	Network     string         `json:"network"`
	HostedOn    string         `json:"hosted_on"`
	DependsOn   []string       `json:"depends_on"`
	Description string         `json:"description"`
	Attributes  map[string]any `json:"attributes"`
}

// Relationship is an explicit digital-twin edge from assets.yaml.
type Relationship struct {
	From string `json:"from"` // employee:<code> | identity:<username> | asset:<code>
	Type string `json:"type"`
	To   string `json:"to"`
}

// World is world.yaml.
type World struct {
	Zones     []Zone     `json:"zones"`
	Locations []Location `json:"locations"`
	Objects   []Object   `json:"objects"`
}

// Zone is a navigable area of the 3D world.
type Zone struct {
	Code               string `json:"code"`
	Name               string `json:"name"`
	Kind               string `json:"kind"`
	Parent             string `json:"parent"`
	Description        string `json:"description"`
	UnlockedByDefault  bool   `json:"unlocked_by_default"`
	RequiredCareerRank int32  `json:"required_career_rank"`
}

// Location is a place inside a zone.
type Location struct {
	Code        string `json:"code"`
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Zone        string `json:"zone"`
	Department  string `json:"department"`
	Description string `json:"description"`
}

// Object is an interactive Three.js object.
type Object struct {
	Key          string         `json:"key"`
	Name         string         `json:"name"`
	Kind         string         `json:"kind"`
	Location     string         `json:"location"`
	Asset        string         `json:"asset"`
	LeadsTo      string         `json:"leads_to"`
	Interactions []string       `json:"interactions"`
	Description  string         `json:"description"`
	Content      string         `json:"content"`
	Attributes   map[string]any `json:"attributes"`
}

// File wrappers for the list-shaped documents.
type (
	departmentsFile struct {
		Departments []Department `json:"departments"`
	}
	employeesFile struct {
		Employees       []Employee       `json:"employees"`
		ServiceAccounts []ServiceAccount `json:"service_accounts"`
	}
	networkFile struct {
		Networks []Network `json:"networks"`
	}
	rangesFile struct {
		Templates []RangeTemplate `json:"templates"`
	}
	assetsFile struct {
		Assets        []Asset        `json:"assets"`
		Relationships []Relationship `json:"relationships"`
	}
)

// RangeTemplate is one entry of ranges.yaml.
type RangeTemplate struct {
	Slug        string                   `json:"slug"`
	Name        string                   `json:"name"`
	Description string                   `json:"description"`
	TTLMinutes  int32                    `json:"ttl_minutes"`
	Workloads   []cyberrange.Workload    `json:"workloads"`
	Network     []cyberrange.NetworkRule `json:"network"`
}

// Spec returns the template's workload specification.
func (t RangeTemplate) Spec() cyberrange.Spec {
	return cyberrange.Spec{Workloads: t.Workloads, Network: t.Network}
}

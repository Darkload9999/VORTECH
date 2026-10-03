package config

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Orphan namespace policies.
const (
	OrphanDelete     = "delete"
	OrphanQuarantine = "quarantine"
)

// RangeConfig configures range provisioning in the worker. Defaults fit
// the single 8 vCPU / 32 GB VPS: 3 vCPU, 12 GiB and 40 GiB of disk stay
// reserved for the platform, plus 10% headroom.
type RangeConfig struct {
	// Kubeconfig is a kubeconfig path; empty means in-cluster credentials.
	Kubeconfig string

	ClusterCPUMillis   int64
	ClusterMemoryMiB   int64
	ClusterStorageMiB  int64
	ReservedCPUMillis  int64
	ReservedMemoryMiB  int64
	ReservedStorageMiB int64
	HeadroomPercent    int64

	// ImageAllowlist holds image reference prefixes ranges may run.
	ImageAllowlist []string
	// OrphanPolicy decides what happens to managed namespaces with no live
	// range: delete them, or quarantine them for inspection.
	OrphanPolicy string

	ProvisionTimeout  time.Duration
	DestroyTimeout    time.Duration
	Concurrency       int
	AdmitInterval     time.Duration
	ExpiryInterval    time.Duration
	ReconcileInterval time.Duration
	// StaleAfter is how long a range may sit in a state that needs a job
	// without one before the reconciler re-drives it.
	StaleAfter time.Duration
}

// LoadRanges reads and validates the range configuration.
func LoadRanges(lookup LookupFunc) (RangeConfig, error) {
	p := parser{lookup: lookup}
	cfg := RangeConfig{
		Kubeconfig:         p.str("KUBECONFIG", ""),
		ClusterCPUMillis:   p.int64("RANGE_CLUSTER_CPU_MILLIS", 8000),
		ClusterMemoryMiB:   p.int64("RANGE_CLUSTER_MEMORY_MIB", 32768),
		ClusterStorageMiB:  p.int64("RANGE_CLUSTER_STORAGE_MIB", 102400),
		ReservedCPUMillis:  p.int64("RANGE_RESERVED_CPU_MILLIS", 3000),
		ReservedMemoryMiB:  p.int64("RANGE_RESERVED_MEMORY_MIB", 12288),
		ReservedStorageMiB: p.int64("RANGE_RESERVED_STORAGE_MIB", 40960),
		HeadroomPercent:    p.int64("RANGE_HEADROOM_PERCENT", 10),
		OrphanPolicy:       p.str("RANGE_ORPHAN_POLICY", OrphanDelete),
		ProvisionTimeout:   p.duration("RANGE_PROVISION_TIMEOUT", 5*time.Minute),
		DestroyTimeout:     p.duration("RANGE_DESTROY_TIMEOUT", 3*time.Minute),
		Concurrency:        int(p.int32("RANGE_WORKER_CONCURRENCY", 4)),
		AdmitInterval:      p.duration("RANGE_ADMIT_INTERVAL", 15*time.Second),
		ExpiryInterval:     p.duration("RANGE_EXPIRY_INTERVAL", 30*time.Second),
		ReconcileInterval:  p.duration("RANGE_RECONCILE_INTERVAL", time.Minute),
		StaleAfter:         p.duration("RANGE_STALE_AFTER", 10*time.Minute),
	}
	allow := p.str("RANGE_IMAGE_ALLOWLIST", "docker.io/library/,docker.io/nginxinc/,docker.io/traefik/,docker.io/gitea/")
	for _, a := range strings.Split(allow, ",") {
		if a = strings.TrimSpace(a); a != "" {
			cfg.ImageAllowlist = append(cfg.ImageAllowlist, a)
		}
	}

	errs := p.errs
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	for _, d := range []struct {
		name            string
		total, reserved int64
	}{
		{"CPU_MILLIS", cfg.ClusterCPUMillis, cfg.ReservedCPUMillis},
		{"MEMORY_MIB", cfg.ClusterMemoryMiB, cfg.ReservedMemoryMiB},
		{"STORAGE_MIB", cfg.ClusterStorageMiB, cfg.ReservedStorageMiB},
	} {
		if d.total <= 0 || d.reserved < 0 || d.reserved >= d.total {
			add("RANGE_CLUSTER_%s / RANGE_RESERVED_%s: cluster must be positive and larger than the reserve", d.name, d.name)
		}
	}
	if cfg.HeadroomPercent < 0 || cfg.HeadroomPercent > 90 {
		add("RANGE_HEADROOM_PERCENT: must be between 0 and 90")
	}
	if len(cfg.ImageAllowlist) == 0 {
		add("RANGE_IMAGE_ALLOWLIST: must list at least one registry/repository prefix")
	}
	for _, a := range cfg.ImageAllowlist {
		// A bare registry ("docker.io") would also match "docker.io.evil.example".
		if !strings.Contains(a, "/") {
			add("RANGE_IMAGE_ALLOWLIST: %q must contain a '/' (e.g. docker.io/library/)", a)
		}
	}
	if cfg.OrphanPolicy != OrphanDelete && cfg.OrphanPolicy != OrphanQuarantine {
		add("RANGE_ORPHAN_POLICY: must be %q or %q", OrphanDelete, OrphanQuarantine)
	}
	if cfg.ProvisionTimeout < 30*time.Second || cfg.ProvisionTimeout > 30*time.Minute {
		add("RANGE_PROVISION_TIMEOUT: must be between 30s and 30m")
	}
	if cfg.DestroyTimeout < 30*time.Second || cfg.DestroyTimeout > 30*time.Minute {
		add("RANGE_DESTROY_TIMEOUT: must be between 30s and 30m")
	}
	if cfg.Concurrency < 1 || cfg.Concurrency > 64 {
		add("RANGE_WORKER_CONCURRENCY: must be between 1 and 64")
	}
	for name, d := range map[string]time.Duration{
		"RANGE_ADMIT_INTERVAL": cfg.AdmitInterval, "RANGE_EXPIRY_INTERVAL": cfg.ExpiryInterval,
		"RANGE_RECONCILE_INTERVAL": cfg.ReconcileInterval,
	} {
		if d < time.Second || d > time.Hour {
			add("%s: must be between 1s and 1h", name)
		}
	}
	if cfg.StaleAfter < cfg.ProvisionTimeout {
		add("RANGE_STALE_AFTER: must be at least RANGE_PROVISION_TIMEOUT")
	}
	if len(errs) > 0 {
		return RangeConfig{}, fmt.Errorf("invalid range configuration:\n%w", errors.Join(errs...))
	}
	return cfg, nil
}

// LogValue implements slog.LogValuer.
func (r RangeConfig) LogValue() slog.Value {
	kube := r.Kubeconfig
	if kube == "" {
		kube = "(in-cluster)"
	}
	return slog.GroupValue(
		slog.String("kubeconfig", kube),
		slog.Int64("cluster_cpu_millis", r.ClusterCPUMillis),
		slog.Int64("cluster_memory_mib", r.ClusterMemoryMiB),
		slog.Int64("reserved_cpu_millis", r.ReservedCPUMillis),
		slog.Int64("reserved_memory_mib", r.ReservedMemoryMiB),
		slog.Int64("headroom_percent", r.HeadroomPercent),
		slog.Any("image_allowlist", r.ImageAllowlist),
		slog.String("orphan_policy", r.OrphanPolicy),
		slog.Duration("provision_timeout", r.ProvisionTimeout),
		slog.Int("concurrency", r.Concurrency),
	)
}

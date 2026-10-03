package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadRanges(t *testing.T) {
	cfg, err := LoadRanges(env(map[string]string{"KUBECONFIG": "/tmp/kc", "RANGE_IMAGE_ALLOWLIST": "docker.io/library/, ghcr.io/vortech/"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Kubeconfig != "/tmp/kc" || len(cfg.ImageAllowlist) != 2 || cfg.OrphanPolicy != OrphanDelete ||
		cfg.ClusterCPUMillis != 8000 || cfg.ReservedCPUMillis != 3000 || cfg.ProvisionTimeout != 5*time.Minute {
		t.Fatalf("unexpected config %+v", cfg)
	}

	for name, e := range map[string]map[string]string{
		"reserve exceeds cluster": {"RANGE_RESERVED_CPU_MILLIS": "9000"},
		"headroom too high":       {"RANGE_HEADROOM_PERCENT": "95"},
		"bare registry":           {"RANGE_IMAGE_ALLOWLIST": "docker.io"},
		"empty allowlist":         {"RANGE_IMAGE_ALLOWLIST": " , "},
		"unknown orphan policy":   {"RANGE_ORPHAN_POLICY": "ignore"},
		"stale before timeout":    {"RANGE_STALE_AFTER": "1m"},
		"zero concurrency":        {"RANGE_WORKER_CONCURRENCY": "0"},
	} {
		if _, err := LoadRanges(env(e)); err == nil || !strings.Contains(err.Error(), "RANGE_") {
			t.Errorf("%s: expected validation error, got %v", name, err)
		}
	}
}

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCreateMigration(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"00001_foundation.sql", "00007_players.sql", "embed.go"} {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	path, err := createMigration(dir, "add_ranges")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "00008_add_ranges.sql" {
		t.Fatalf("path = %s, want 00008_add_ranges.sql", path)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != migrationTemplate {
		t.Fatalf("unexpected content %q err=%v", b, err)
	}

	if _, err := createMigration(dir, "add_ranges"); err == nil {
		t.Fatal("duplicate migration name should be rejected")
	}
	for _, bad := range []string{"", "Add-Ranges", "../escape", "1starts_with_digit"} {
		if _, err := createMigration(dir, bad); err == nil {
			t.Errorf("name %q should be rejected", bad)
		}
	}
}

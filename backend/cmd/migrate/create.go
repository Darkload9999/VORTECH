package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// migrationsDir is relative to the backend module root, where make runs.
const migrationsDir = "migrations"

var (
	migrationName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
	migrationFile = regexp.MustCompile(`^(\d{5})_[a-z0-9_]+\.sql$`)
)

const migrationTemplate = `-- +goose Up


-- +goose Down

`

// createMigration writes dir/NNNNN_name.sql using the next sequential version.
// Sequential (not timestamp) versions keep ordering explicit in review.
func createMigration(dir, name string) (string, error) {
	if !migrationName.MatchString(name) {
		return "", errors.New("migration name must be lowercase snake_case, e.g. add_players")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("read %s (run from the backend directory): %w", dir, err)
	}
	next := 1
	for _, e := range entries {
		m := migrationFile.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		v, _ := strconv.Atoi(m[1])
		if v >= next {
			next = v + 1
		}
		if strings.HasSuffix(strings.TrimSuffix(e.Name(), ".sql"), "_"+name) {
			return "", fmt.Errorf("a migration named %q already exists: %s", name, e.Name())
		}
	}
	if next > 99999 {
		return "", errors.New("migration version space exhausted")
	}
	path := filepath.Join(dir, fmt.Sprintf("%05d_%s.sql", next, name))
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(migrationTemplate); err != nil {
		_ = f.Close()
		return "", err
	}
	return path, f.Close()
}

package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/Darkload9999/VORTECH/backend/migrations"
)

// ErrNoMigrationToRollback is returned by Down when the schema is empty.
var ErrNoMigrationToRollback = errors.New("no applied migration to roll back")

// Migrator applies the embedded schema migrations.
//
// It uses its own short-lived connection rather than the application pool
// because migrations need different session settings: no statement timeout
// (index builds may be slow) but a lock timeout, so a migration waiting for
// an ACCESS EXCLUSIVE lock fails instead of stalling live traffic queued
// behind it. A PostgreSQL advisory lock serialises concurrent migrators
// (e.g. API and worker starting together).
type Migrator struct {
	db       *sql.DB
	provider *goose.Provider
	log      *slog.Logger
}

// NewMigrator opens a dedicated connection for migrations.
func NewMigrator(databaseURL string, log *slog.Logger) (*Migrator, error) {
	connCfg, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return nil, errors.New("parse DATABASE_URL: invalid connection string")
	}
	rp := connCfg.RuntimeParams
	rp["application_name"] = "vortech-migrate"
	rp["statement_timeout"] = "0"
	rp["lock_timeout"] = "15000"
	rp["timezone"] = "UTC"

	db := stdlib.OpenDB(*connCfg)
	db.SetMaxOpenConns(2)

	// Wait up to 5 minutes (every 5s, 60 attempts) for another migrator.
	locker, err := lock.NewPostgresSessionLocker(lock.WithLockTimeout(5, 60))
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create migration locker: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS,
		goose.WithSessionLocker(locker),
	)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create migration provider: %w", err)
	}
	return &Migrator{db: db, provider: provider, log: log}, nil
}

// Up applies all pending migrations.
func (m *Migrator) Up(ctx context.Context) error {
	results, err := m.provider.Up(ctx)
	for _, r := range results {
		m.logResult(r)
	}
	if err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	if len(results) == 0 {
		m.log.Info("database schema is up to date")
	}
	return nil
}

// Down rolls back the most recently applied migration.
func (m *Migrator) Down(ctx context.Context) error {
	r, err := m.provider.Down(ctx)
	if errors.Is(err, goose.ErrNoNextVersion) {
		return ErrNoMigrationToRollback
	}
	if r != nil {
		m.logResult(r)
	}
	if err != nil {
		return fmt.Errorf("roll back migration: %w", err)
	}
	return nil
}

// MigrationStatus describes one known migration.
type MigrationStatus struct {
	Version int64
	Name    string
	Applied bool
}

// Status lists all embedded migrations and whether each is applied.
func (m *Migrator) Status(ctx context.Context) ([]MigrationStatus, error) {
	st, err := m.provider.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("migration status: %w", err)
	}
	out := make([]MigrationStatus, len(st))
	for i, s := range st {
		out[i] = MigrationStatus{
			Version: s.Source.Version,
			Name:    s.Source.Path,
			Applied: s.State == goose.StateApplied,
		}
	}
	return out, nil
}

// Versions returns the current database version and the latest embedded one.
func (m *Migrator) Versions(ctx context.Context) (current, target int64, err error) {
	current, target, err = m.provider.GetVersions(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("read schema versions: %w", err)
	}
	return current, target, nil
}

// EnsureUpToDate returns an error if migrations are pending. It does not
// take the migration lock, so it never blocks behind a running migration.
func (m *Migrator) EnsureUpToDate(ctx context.Context) error {
	pending, err := m.provider.HasPending(ctx)
	if err != nil {
		return fmt.Errorf("check pending migrations: %w", err)
	}
	current, target, err := m.Versions(ctx)
	if err != nil {
		return err
	}
	if pending {
		return fmt.Errorf("database schema is at version %d but this build requires %d: run the migrate job (or set DATABASE_AUTO_MIGRATE=true outside production)", current, target)
	}
	if current > target {
		// Expected briefly during a rollback of the application; migrations
		// are written expand/contract so N-1 code tolerates schema N.
		m.log.Warn("database schema is newer than this build", "db_schema_version", current, "build_schema_version", target)
	}
	return nil
}

// Close releases the migration connection.
func (m *Migrator) Close() error {
	return m.db.Close()
}

func (m *Migrator) logResult(r *goose.MigrationResult) {
	attrs := []any{
		"migration_version", r.Source.Version,
		"file", r.Source.Path,
		"direction", r.Direction,
		"duration", r.Duration,
	}
	if r.Error != nil {
		m.log.Error("migration failed", append(attrs, "error", r.Error)...)
		return
	}
	m.log.Info("migration applied", attrs...)
}

// PrepareSchema applies migrations when autoMigrate is set, otherwise it
// verifies the schema is current. Services call it at startup so they never
// run against a schema they were not built for.
func PrepareSchema(ctx context.Context, databaseURL string, autoMigrate bool, log *slog.Logger) error {
	m, err := NewMigrator(databaseURL, log)
	if err != nil {
		return err
	}
	defer m.Close()

	if autoMigrate {
		return m.Up(ctx)
	}
	return m.EnsureUpToDate(ctx)
}

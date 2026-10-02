// Command migrate manages the PostgreSQL schema.
//
//	migrate up        apply all pending migrations
//	migrate down      roll back the most recent migration
//	migrate status    list migrations and whether each is applied
//	migrate version   print the current and latest schema versions
//	migrate create <name>  scaffold the next sequential migration file
//
// In production this runs as a one-off job before the API rollout.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"

	"github.com/vortech/backend/internal/config"
	"github.com/vortech/backend/internal/database"
	"github.com/vortech/backend/internal/logging"
)

const usage = `usage: migrate <command>

commands:
  up        apply all pending migrations
  down      roll back the most recent migration
  status    list migrations and whether each is applied
  version   print the current and latest schema versions
  create    scaffold the next migration: migrate create <name>`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 2 && args[0] == "create" {
		// Scaffolding needs neither configuration nor a database.
		path, err := createMigration(migrationsDir, args[1])
		if err != nil {
			return err
		}
		fmt.Println("created", path)
		return nil
	}
	if len(args) != 1 {
		return errors.New(usage)
	}

	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		return err
	}
	log := logging.New(os.Stderr, cfg.Log, "migrate")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	m, err := database.NewMigrator(cfg.Database.URL, log)
	if err != nil {
		return err
	}
	defer m.Close()

	switch args[0] {
	case "up":
		return m.Up(ctx)
	case "down":
		return m.Down(ctx)
	case "status":
		st, err := m.Status(ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "VERSION\tSTATE\tFILE")
		for _, s := range st {
			state := "pending"
			if s.Applied {
				state = "applied"
			}
			fmt.Fprintf(tw, "%d\t%s\t%s\n", s.Version, state, s.Name)
		}
		return tw.Flush()
	case "version":
		current, target, err := m.Versions(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("current=%d latest=%d\n", current, target)
		return nil
	default:
		return fmt.Errorf("unknown command %q\n\n%s", args[0], usage)
	}
}

// Command scenario validates and imports scenario-as-code directories.
//
//	scenario validate <dir>                     schema + cross-reference checks, no database
//	scenario import [-activate] [-force] <dir>  validate, then upsert into PostgreSQL
//	scenario schema                             print the JSON Schema (for editors)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"github.com/Darkload9999/VORTECH/backend/internal/config"
	"github.com/Darkload9999/VORTECH/backend/internal/database"
	"github.com/Darkload9999/VORTECH/backend/internal/logging"
	"github.com/Darkload9999/VORTECH/backend/internal/scenario"
)

const usage = `usage:
  scenario validate <dir>
  scenario import [-activate] [-force] [-actor name] <dir>
  scenario schema`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "schema":
		_, err := os.Stdout.Write(scenario.SchemaJSON())
		return err
	case "validate":
		if len(args) != 2 {
			return errors.New(usage)
		}
		b, err := scenario.Load(args[1])
		if err != nil {
			return err
		}
		fmt.Printf("%s %s is valid (content %s)\n", b.Scenario.Slug, b.Scenario.Version, b.Hash[:12])
		return nil
	case "import":
		return runImport(args[1:])
	default:
		return fmt.Errorf("unknown command %q\n\n%s", args[0], usage)
	}
}

func runImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	activate := fs.Bool("activate", false, "publish the scenario and serve its world")
	force := fs.Bool("force", false, "re-apply even if the content is unchanged")
	actor := fs.String("actor", "scenario-cli", "actor recorded in the audit log")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New(usage)
	}

	b, err := scenario.Load(fs.Arg(0))
	if err != nil {
		return err
	}

	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		return err
	}
	log := logging.New(os.Stderr, cfg.Log, "scenario")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	pool, err := database.Connect(ctx, cfg.Database, "vortech-scenario", log)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := database.PrepareSchema(ctx, cfg.Database.URL, cfg.Database.AutoMigrate, log); err != nil {
		return err
	}

	res, err := scenario.Import(ctx, pool, b, scenario.ImportOptions{Activate: *activate, Force: *force, Actor: *actor})
	if err != nil {
		return err
	}
	fmt.Printf("%s %s: %s (scenario %s, company %s, activated=%t)\n",
		b.Scenario.Slug, b.Scenario.Version, res.Outcome, res.ScenarioID, res.CompanyID, res.Activated)
	printCounts("imported", res.Counts)
	printCounts("pruned", res.Pruned)
	return nil
}

func printCounts[V int | int64](label string, m map[string]V) {
	if len(m) == 0 {
		return
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Printf("  %s:", label)
	for _, k := range keys {
		fmt.Printf(" %s=%d", k, m[k])
	}
	fmt.Println()
}

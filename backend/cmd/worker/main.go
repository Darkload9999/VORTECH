// Command worker runs asynchronous background work outside the request
// path: the durable job queue (range admission, provisioning, teardown)
// and the range expiry and reconciliation loops against Kubernetes.
//
// Several workers may run at once: jobs are claimed with SKIP LOCKED
// leases, admission is serialised by an advisory lock, and every range
// transition is a compare-and-set.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/Darkload9999/VORTECH/backend/internal/buildinfo"
	"github.com/Darkload9999/VORTECH/backend/internal/config"
	"github.com/Darkload9999/VORTECH/backend/internal/cyberrange"
	"github.com/Darkload9999/VORTECH/backend/internal/cyberrange/controller"
	"github.com/Darkload9999/VORTECH/backend/internal/cyberrange/kube"
	"github.com/Darkload9999/VORTECH/backend/internal/database"
	"github.com/Darkload9999/VORTECH/backend/internal/health"
	"github.com/Darkload9999/VORTECH/backend/internal/httpx"
	"github.com/Darkload9999/VORTECH/backend/internal/jobs"
	"github.com/Darkload9999/VORTECH/backend/internal/logging"
	"github.com/Darkload9999/VORTECH/backend/internal/server"
)

const (
	pathHealth = "/health"
	pathReady  = "/ready"
)

func main() {
	healthcheck := flag.Bool("healthcheck", false, "probe the local liveness endpoint and exit (container HEALTHCHECK)")
	flag.Parse()

	if *healthcheck {
		addr, ok := os.LookupEnv("WORKER_HTTP_ADDR")
		if !ok || addr == "" {
			addr = ":8081"
		}
		if err := server.Probe(context.Background(), addr, pathHealth); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "worker: fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		return err
	}
	log := logging.New(os.Stdout, cfg.Log, "worker")
	log.Info("starting", "commit", buildinfo.Commit, "build_time", buildinfo.BuildTime, "config", cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	context.AfterFunc(ctx, stop)

	pool, err := database.Connect(ctx, cfg.Database, "vortech-worker", log)
	if err != nil {
		return err
	}
	defer func() {
		pool.Close()
		log.Info("postgres pool closed")
	}()

	if err := database.PrepareSchema(ctx, cfg.Database.URL, cfg.Database.AutoMigrate, log); err != nil {
		return err
	}

	rangeCfg, err := config.LoadRanges(os.LookupEnv)
	if err != nil {
		return err
	}
	log.Info("range configuration", "ranges", rangeCfg)
	cluster, err := kube.NewKubeCluster(rangeCfg.Kubeconfig)
	if err != nil {
		return err
	}

	hs := health.New(log, cfg.Health.CheckTimeout, cfg.Health.CacheTTL,
		health.Check{Name: "postgres", Fn: database.PingCheck(pool)},
		health.Check{Name: "kubernetes", Fn: cluster.Ping},
	)

	ctrl := controller.New(pool, cluster, log, controller.Config{
		Capacity: cyberrange.Capacity{
			Cluster:         cyberrange.Resources{CPUMillis: rangeCfg.ClusterCPUMillis, MemoryMiB: rangeCfg.ClusterMemoryMiB, StorageMiB: rangeCfg.ClusterStorageMiB},
			Reserved:        cyberrange.Resources{CPUMillis: rangeCfg.ReservedCPUMillis, MemoryMiB: rangeCfg.ReservedMemoryMiB, StorageMiB: rangeCfg.ReservedStorageMiB},
			HeadroomPercent: rangeCfg.HeadroomPercent,
		},
		ImageAllowlist:   rangeCfg.ImageAllowlist,
		OrphanPolicy:     rangeCfg.OrphanPolicy,
		ProvisionTimeout: rangeCfg.ProvisionTimeout,
		DestroyTimeout:   rangeCfg.DestroyTimeout,
		StaleAfter:       rangeCfg.StaleAfter,
	})
	runner := jobs.NewRunner(pool, log, jobs.Config{Concurrency: rangeCfg.Concurrency, Grace: cfg.Shutdown.Timeout / 2})
	ctrl.Register(runner)

	// Background work stops when ctx ends (SIGTERM); in-flight jobs get a
	// grace period and are otherwise released to another worker.
	var bg sync.WaitGroup
	bg.Go(func() {
		if err := runner.Run(ctx); err != nil {
			log.Error("job runner stopped", "error", err)
		}
	})
	bg.Go(func() {
		ctrl.RunLoops(ctx, controller.Intervals{
			Admit: rangeCfg.AdmitInterval, Expiry: rangeCfg.ExpiryInterval, Reconcile: rangeCfg.ReconcileInterval,
		})
	})

	ln, err := server.Listen(cfg.Worker.HTTPAddr)
	if err != nil {
		return err
	}
	srv := server.New(cfg.Worker.HTTPAddr, probeHandler(log, hs), cfg.HTTP, log)

	log.Info("worker ready", "worker_id", runner.WorkerID())

	err = server.Run(ctx, log, srv, ln, server.ShutdownOptions{
		OnDrain: hs.SetDraining,
		Timeout: cfg.Shutdown.Timeout,
	})
	bg.Wait()
	if err != nil {
		return err
	}
	log.Info("shutdown complete")
	return nil
}

// probeHandler serves the internal-only probe endpoints.
func probeHandler(log *slog.Logger, hs *health.Service) http.Handler {
	r := httpx.NewRouter()
	r.HandleFunc("GET "+pathHealth, hs.Live)
	r.HandleFunc("GET "+pathReady, hs.Ready)
	return httpx.Chain(r, httpx.RequestID, httpx.AccessLog(log, pathHealth, pathReady), httpx.Recover(log))
}

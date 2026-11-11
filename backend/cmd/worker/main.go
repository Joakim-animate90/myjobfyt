// Command worker runs the MyJobFyt event consumers.
//
// Consumers are selected by group (WORKER_GROUPS), so groups can be deployed
// separately. That separation is the point: an AI-provider outage stalls the cv group
// while applications keeps submitting, and a discovery backlog cannot starve
// submission.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/config"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/cv"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/antivirus"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/docextract"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/events"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/postgres"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/storage"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/telemetry"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/worker"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "worker: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := telemetry.NewLogger(cfg.Log, cfg.ServiceName+"-worker", cfg.Version)
	slog.SetDefault(log)

	log.Info("starting worker",
		slog.String("env", string(cfg.Env)),
		slog.Any("groups", cfg.Worker.Groups),
		slog.Int("concurrency", cfg.Worker.Concurrency),
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// --- dependencies -------------------------------------------------------

	db, err := postgres.Open(ctx, cfg.Postgres)
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer db.Close()

	objectStore, err := storage.Open(ctx, cfg.Storage)
	if err != nil {
		return fmt.Errorf("object storage: %w", err)
	}

	bus, err := events.Connect(ctx, cfg.NATS, log)
	if err != nil {
		return fmt.Errorf("nats: %w", err)
	}
	defer func() { _ = bus.Close() }()

	scanner, err := buildScanner(ctx, cfg, log)
	if err != nil {
		return err
	}

	log.Info("dependencies ready",
		slog.String("scanner", scanner.Name()),
		slog.String("bucket", cfg.Storage.Bucket),
	)

	// --- consumers ----------------------------------------------------------

	cvWorker := cv.NewWorker(cv.WorkerDeps{
		Repo:    cv.NewPostgresRepository(db),
		Store:   objectStore,
		Scanner: scanner,
		Bus:     bus,
		Logger:  log,
		Limits: docextract.Limits{
			MaxPages: cfg.Extract.MaxPages,
			MaxChars: cfg.Extract.MaxChars,
		},
	})

	registrations, err := worker.Collect(cvWorker)
	if err != nil {
		return fmt.Errorf("assemble consumers: %w", err)
	}
	unmatched, err := worker.ValidateGroups(registrations, cfg.Worker.Groups)
	if err != nil {
		return err
	}
	if len(unmatched) > 0 {
		// Expected while a domain is still being built, but logged so a genuine typo
		// in WORKER_GROUPS is visible rather than silent.
		log.Warn("configured groups have no consumers yet",
			slog.Any("groups", unmatched),
			slog.Any("available", worker.Groups(registrations)),
		)
	}

	// Idempotency claims live in Postgres rather than Redis: an evicted key would
	// silently re-enable a duplicate side effect. Retention must exceed the longest
	// stream retention so a replayed message cannot outlive the record suppressing it.
	idempotency := events.NewPostgresIdempotencyStore(db, 45*24*time.Hour)

	runtime := events.NewRuntime(bus, idempotency, log, cfg.Worker.AckWait, cfg.Worker.MaxDeliver)
	if err := runtime.Start(ctx, registrations, cfg.Worker.Groups); err != nil {
		return fmt.Errorf("start consumers: %w", err)
	}

	metricsServer := startMetricsServer(cfg, log)

	<-ctx.Done()

	log.Info("shutdown signal received; draining consumers",
		slog.Duration("grace", cfg.Worker.ShutdownTO))

	// Drain rather than stop, so in-flight handlers finish instead of having their
	// work abandoned and redelivered. A rolling deploy should not re-do work.
	runtime.Stop(cfg.Worker.ShutdownTO)

	if metricsServer != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = metricsServer.Shutdown(shutdownCtx)
	}

	log.Info("shutdown complete")
	return nil
}

// buildScanner selects the antivirus implementation.
//
// Development gets a pass-through scanner so the pipeline runs without a 300-second
// signature download on every machine. Production refuses to start without a real one:
// an unconditional pass is indistinguishable from having no scanner, on an endpoint
// that accepts arbitrary files from the internet.
func buildScanner(ctx context.Context, cfg *config.Config, log *slog.Logger) (cv.Scanner, error) {
	if cfg.Antivirus.Addr == "" {
		if cfg.Env.IsProduction() {
			return nil, errors.New("ANTIVIRUS_ADDR is required")
		}
		return antivirus.NewDevelopmentScanner(log), nil
	}

	scanner := antivirus.NewClamAV(antivirus.ClamAVConfig{
		Addr:     cfg.Antivirus.Addr,
		Timeout:  cfg.Antivirus.Timeout,
		MaxBytes: cfg.Antivirus.MaxBytes,
	})

	pingCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := scanner.Ping(pingCtx); err != nil {
		// Fail at boot rather than on the first upload. ClamAV takes minutes to load
		// its database on a cold start, and a worker that starts healthy and then
		// errors on every scan is much harder to diagnose than one that refuses to
		// start.
		return nil, fmt.Errorf("antivirus scanner at %s is not responding: %w", cfg.Antivirus.Addr, err)
	}

	return scanner, nil
}

// startMetricsServer exposes Prometheus metrics and a liveness endpoint.
func startMetricsServer(cfg *config.Config, log *slog.Logger) *http.Server {
	if !cfg.Telemetry.Enabled || cfg.Telemetry.MetricsAddr == "" {
		return nil
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	// A worker has no request surface, so liveness is simply "the process is running".
	// Consumer health is reported through metrics and dead-letter depth, which is where
	// a stalled consumer actually shows up.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	srv := &http.Server{
		Addr:              cfg.Telemetry.MetricsAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Info("worker metrics listening", slog.String("addr", cfg.Telemetry.MetricsAddr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("worker metrics server stopped", slog.Any("error", err))
		}
	}()

	return srv
}

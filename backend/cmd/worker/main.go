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
	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/entitlements"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/jobs"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/jobs/sources"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/ai"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/antivirus"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/docextract"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/embed"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/events"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/mailer"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/postgres"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/redisx"
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

	// Installs the global tracer provider. Without it otel.Tracer() returns a no-op:
	// every span is discarded and the event envelopes carry empty trace context, so
	// the pipeline looks instrumented and records nothing.
	shutdownTracing, err := telemetry.SetupTracing(ctx, telemetry.TracingOptions{
		ServiceName:  cfg.ServiceName + "-worker",
		Version:      cfg.Version,
		Environment:  string(cfg.Env),
		OTLPEndpoint: cfg.Telemetry.OTLPEndpoint,
		SampleRatio:  cfg.Telemetry.TraceSampleRatio,
		Insecure:     cfg.Telemetry.OTLPInsecure,
	}, log)
	if err != nil {
		return fmt.Errorf("set up tracing: %w", err)
	}
	defer func() {
		// A fresh context: ctx is already cancelled by the time this runs, and a
		// cancelled context exports nothing — losing exactly the spans from the
		// shutdown someone is investigating.
		flushCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := shutdownTracing(flushCtx); err != nil {
			log.Error("could not flush traces on shutdown", slog.Any("error", err))
		}
	}()

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

	extractor, err := buildExtractor(ctx, cfg, db, log)
	if err != nil {
		return err
	}

	embedder, err := buildEmbedder(ctx, cfg, log)
	if err != nil {
		return err
	}

	log.Info("dependencies ready",
		slog.String("scanner", scanner.Name()),
		slog.String("bucket", cfg.Storage.Bucket),
		slog.Bool("structured_extraction", extractor != nil),
		slog.Bool("semantic_matching", embedder != nil),
	)
	if embedder == nil {
		// Worth saying, because the symptom is subtler than an outright failure:
		// matching still works but scores lexically, and every affected match is
		// marked degraded until an embedding service appears.
		log.Warn("no embedding service configured (EMBED_BASE_URL unset); " +
			"matching will use the lexical fallback and mark matches degraded")
	}
	if extractor == nil {
		// Said out loud, because the symptom otherwise is CVs that parse successfully
		// and produce no matchable data, which looks like a matching bug.
		log.Warn("structured CV extraction is DISABLED (AI_ENABLED=false); " +
			"CVs will be marked parsed on extracted text alone and will not be matchable")
	}

	// --- consumers ----------------------------------------------------------

	// Without a sender a parse failure is silent to the candidate: the version is
	// marked failed, the reason is recorded for the API, and nobody tells them. The
	// logging sender keeps development honest by printing what would have been sent.
	var emailSender mailer.Sender
	if cfg.SMTP.Host == "" {
		emailSender = mailer.NewLoggingSender(log)
		log.Warn("no SMTP host configured; CV parse-failure notices will be logged rather than sent")
	} else {
		emailSender = mailer.NewSMTPSender(cfg.SMTP, log)
	}

	cvWorker := cv.NewWorker(cv.WorkerDeps{
		Repo:      cv.NewPostgresRepository(db),
		Store:     objectStore,
		Scanner:   scanner,
		Extractor: extractor,
		Embedder:  embedder,
		Notifier:  mailer.NewCVMailer(emailSender, cfg.SMTP.AppBaseURL, log),
		Bus:       bus,
		Logger:    log,
		Limits: docextract.Limits{
			MaxPages: cfg.Extract.MaxPages,
			MaxChars: cfg.Extract.MaxChars,
		},
	})

	// Job sources are assembled here so the startup log names which adapters this
	// deployment can actually use, and which are enabled in the database with nothing
	// to talk to them.
	sourceRegistry, err := sources.BuildRegistry(cfg, log)
	if err != nil {
		return fmt.Errorf("build job source registry: %w", err)
	}
	sourceRecords, err := jobs.NewPostgresSourceRepository(db).EnabledSources(ctx)
	if err != nil {
		return fmt.Errorf("load job sources: %w", err)
	}
	boundSources, unboundSources := sourceRegistry.Bind(sourceRecords)
	sources.ReportUnboundSources(unboundSources, log)
	log.Info("job sources ready",
		slog.Int("usable", len(boundSources)),
		slog.Int("enabled_without_adapter", len(unboundSources)),
	)

	if !cvWorker.CandidateNotificationsEnabled() {
		log.Warn("candidate notifications are DISABLED; a CV that fails to parse will be " +
			"recorded as failed but the candidate will never be told")
	}

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

	// The embedding backfill is what makes an embedding outage temporary. Without it
	// a version the provider was down for would stay unembedded for good and the
	// candidate would be permanently matched on the weaker lexical signal.
	backfillDone := make(chan struct{})
	if embedder != nil {
		go func() {
			defer close(backfillDone)
			if err := cvWorker.RunEmbeddingBackfill(ctx, cv.EmbeddingBackfillConfig{
				Interval:  cfg.Embed.BackfillInterval,
				BatchSize: cfg.Embed.BackfillBatch,
			}); err != nil {
				log.Error("embedding backfill stopped", slog.Any("error", err))
			}
		}()
	} else {
		close(backfillDone)
	}

	// A dead letter is candidate work the platform has given up on, so "anything is
	// waiting for a human" has to be observable. The arrival counter cannot answer that
	// on its own: it only ever increases.
	dlqMonitorDone := make(chan struct{})
	go func() {
		defer close(dlqMonitorDone)
		monitor := events.NewDeadLetterMonitor(bus.JetStream(), log)
		if err := monitor.Run(ctx, events.DeadLetterMonitorConfig{
			Interval: cfg.Worker.DLQPollInterval,
		}); err != nil {
			log.Error("dead-letter monitor stopped", slog.Any("error", err))
		}
	}()

	metricsServer := startMetricsServer(cfg, log)

	<-ctx.Done()

	log.Info("shutdown signal received; draining consumers",
		slog.Duration("grace", cfg.Worker.ShutdownTO))

	// Drain rather than stop, so in-flight handlers finish instead of having their
	// work abandoned and redelivered. A rolling deploy should not re-do work.
	runtime.Stop(cfg.Worker.ShutdownTO)

	// The backfill observes the same cancelled context; wait so a sweep in progress
	// finishes its current version rather than being abandoned mid-write.
	<-backfillDone
	<-dlqMonitorDone

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

// buildExtractor constructs the structured-extraction adapter, or nil when AI is
// disabled.
//
// Returning nil rather than erroring is deliberate: the pipeline runs end to end
// without a provider, which keeps the platform usable with no API key and no spend.
// Production validation makes that impossible there, because a CV that never becomes
// matchable means the product does not work.
func buildExtractor(ctx context.Context, cfg *config.Config, db *postgres.DB, log *slog.Logger) (cv.Extractor, error) {
	if !cfg.AI.Enabled {
		if cfg.Env.IsProduction() {
			return nil, errors.New("AI_ENABLED must be true")
		}
		return nil, nil
	}

	provider := ai.NewAnthropicProvider(ai.AnthropicConfig{
		APIKey:     cfg.AI.APIKey,
		BaseURL:    cfg.AI.BaseURL,
		MaxRetries: cfg.AI.MaxRetries,
	})

	// Reachability is checked at boot rather than on the first CV. A worker that starts
	// healthy and then fails every extraction is much harder to diagnose than one that
	// refuses to start, and the check costs a single token.
	pingCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := provider.Ping(pingCtx); err != nil {
		return nil, fmt.Errorf("ai provider is not reachable: %w", err)
	}

	redis, err := redisx.Open(ctx, cfg.Redis)
	if err != nil {
		return nil, fmt.Errorf("redis (needed for AI token quotas): %w", err)
	}

	accountant := entitlements.NewTokenAccountant(
		entitlements.NewService(entitlements.NewPostgresRepository(db), redis, log, nil),
		log,
	)

	client := ai.NewClient(ai.ClientDeps{
		Provider:   provider,
		Accountant: accountant,
		Logger:     log,
	})

	log.Info("structured CV extraction enabled",
		slog.String("provider", provider.Name()),
		slog.Int64("per_call_token_budget", cfg.AI.ExtractionTokenBudget),
	)

	return cv.NewAIExtractor(client, cv.ExtractorConfig{
		PerCallTokenBudget: cfg.AI.ExtractionTokenBudget,
	}), nil
}

// buildEmbedder constructs the embedding client, or nil when none is configured.
//
// Unlike the AI extractor, a missing embedder is acceptable even in production: the
// lexical fallback is a designed path, not a degradation of last resort, and matching
// works without it. What is not acceptable is an embedder that is configured and
// broken, so a configured one is checked at boot.
func buildEmbedder(ctx context.Context, cfg *config.Config, log *slog.Logger) (cv.Embedder, error) {
	if cfg.Embed.BaseURL == "" {
		return nil, nil
	}

	embedder := embed.NewHTTPEmbedder(embed.HTTPEmbedderConfig{
		BaseURL:  cfg.Embed.BaseURL,
		Timeout:  cfg.Embed.Timeout,
		MaxBatch: cfg.Embed.MaxBatch,
	})

	// Ping embeds a real string rather than hitting a health endpoint, because a
	// service can be listening and unable to load its model.
	pingCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := embedder.Ping(pingCtx); err != nil {
		return nil, fmt.Errorf("embedding service at %s is not responding: %w", cfg.Embed.BaseURL, err)
	}

	log.Info("semantic matching enabled",
		slog.String("embedder", embedder.Name()),
		slog.String("base_url", cfg.Embed.BaseURL),
	)
	return embedder, nil
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

// Command api serves the MyJobFyt REST API.
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
	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/identity"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/httpx"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/ai"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/dataenc"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/events"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/mailer"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/postgres"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/redisx"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/storage"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/telemetry"
	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "api: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := telemetry.NewLogger(cfg.Log, cfg.ServiceName+"-api", cfg.Version)
	slog.SetDefault(log)

	log.Info("starting api",
		slog.String("env", string(cfg.Env)),
		slog.String("version", cfg.Version),
		slog.String("addr", cfg.HTTP.Addr),
	)

	// Signals are wired before any dependency is opened, so a Ctrl-C during a slow
	// startup is honoured instead of leaving half-open connections behind.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Installs the global tracer provider. Without it otel.Tracer() returns a no-op:
	// every span is discarded and the event envelopes carry empty trace context, so
	// the pipeline looks instrumented and records nothing.
	shutdownTracing, err := telemetry.SetupTracing(ctx, telemetry.TracingOptions{
		ServiceName:  cfg.ServiceName + "-api",
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
	log.Info("postgres connected", slog.Int("max_conns", int(cfg.Postgres.MaxConns)))

	redis, err := redisx.Open(ctx, cfg.Redis)
	if err != nil {
		return fmt.Errorf("redis: %w", err)
	}
	defer func() { _ = redis.Close() }()
	log.Info("redis connected", slog.String("addr", cfg.Redis.Addr))

	bus, err := events.Connect(ctx, cfg.NATS, log)
	if err != nil {
		return fmt.Errorf("nats: %w", err)
	}
	defer func() { _ = bus.Close() }()
	log.Info("nats jetstream connected", slog.String("url", cfg.NATS.URL))

	objectStore, err := storage.Open(ctx, cfg.Storage)
	if err != nil {
		return fmt.Errorf("object storage: %w", err)
	}
	log.Info("object storage ready",
		slog.String("endpoint", cfg.Storage.Endpoint),
		slog.String("bucket", cfg.Storage.Bucket),
	)

	cipher, err := buildCipher(cfg, log)
	if err != nil {
		return err
	}

	tokens, err := identity.NewTokenIssuer(cfg.Auth, cfg.Env)
	if err != nil {
		return fmt.Errorf("auth tokens: %w", err)
	}
	if cfg.Auth.JWTPrivateKeyPEM == "" {
		// Worth saying out loud: an ephemeral key means every restart invalidates
		// every session, which is confusing if you do not know it is happening.
		log.Warn("no JWT signing key configured; using an ephemeral development key — all sessions end on restart")
	}

	var emailSender mailer.Sender
	if cfg.SMTP.Host == "" {
		emailSender = mailer.NewLoggingSender(log)
	} else {
		emailSender = mailer.NewSMTPSender(cfg.SMTP, log)
	}
	identityMailer := mailer.NewIdentityMailer(emailSender, cfg.SMTP.AppBaseURL, log)

	// --- domains ------------------------------------------------------------

	entitlementsService := entitlements.NewService(
		entitlements.NewPostgresRepository(db), redis, log, nil,
	)

	identityRepo := identity.NewPostgresRepository(db)
	identityService := identity.NewService(identity.ServiceDeps{
		Repo:   identityRepo,
		Tx:     db,
		Hasher: identity.NewPasswordHasher(cfg.Auth),
		Tokens: tokens,
		Cipher: cipher,
		Bus:    bus,
		Config: cfg.Auth,
		Logger: log,
	})
	identityHandler := identity.NewHandler(
		identityService, identityMailer, cfg.Auth, cfg.Env, cfg.HTTP.TrustedProxy, log,
	)

	cvService := cv.NewService(cv.ServiceDeps{
		Repo:         cv.NewPostgresRepository(db),
		Store:        objectStore,
		Entitlements: entitlementsService,
		Bus:          bus,
		Logger:       log,
	})
	cvHandler := cv.NewHandler(cvService, cfg.Storage.MaxCVBytes, log)

	// Tailoring is the only CV feature that needs the AI provider on the request path.
	// A nil service disables its route rather than failing the whole API, so every
	// other CV operation keeps working without credentials.
	tailoringService, err := buildTailoringService(ctx, cfg, db, log)
	if err != nil {
		return err
	}
	tailoringHandler := cv.NewTailoringHandler(tailoringService, log)
	if tailoringService == nil {
		log.Warn("CV tailoring is DISABLED (AI_ENABLED=false); the tailor endpoint will refuse requests")
	}

	// --- http ---------------------------------------------------------------

	requireAuth := httpx.RequireAuth(identityHandler, identity.AudienceAPI)

	router := httpx.NewRouter(httpx.RouterDeps{
		Config:  cfg,
		Logger:  log,
		Limiter: redis,
		Health: []httpx.HealthChecker{
			httpx.CheckFunc{DependencyName: "postgres", Fn: db.Ping},
			httpx.CheckFunc{DependencyName: "redis", Fn: redis.Ping},
			httpx.CheckFunc{DependencyName: "nats", Fn: func(context.Context) error {
				if !bus.Healthy() {
					return errors.New("jetstream connection is down")
				}
				return nil
			}},
			httpx.CheckFunc{DependencyName: "object_storage", Fn: objectStore.Ping},
		},
		Mount: func(r chi.Router) {
			identityHandler.RegisterRoutes(r, identity.RouteDeps{
				RequireAuth: requireAuth,
				// Applied by the handler to the credential endpoints only, not to the
				// whole domain.
				CredentialRateLimit: httpx.AuthRateLimit(redis, cfg.HTTP.TrustedProxy),
			})
			cvHandler.RegisterRoutes(r, requireAuth)
			tailoringHandler.RegisterRoutes(r, requireAuth)
		},
	})

	server := &http.Server{
		Addr:              cfg.HTTP.Addr,
		Handler:           router,
		ReadTimeout:       cfg.HTTP.ReadTimeout,
		WriteTimeout:      cfg.HTTP.WriteTimeout,
		IdleTimeout:       cfg.HTTP.IdleTimeout,
		ReadHeaderTimeout: 10 * time.Second,
	}

	metricsServer := startMetricsServer(cfg, log)

	serverErrors := make(chan error, 1)
	go func() {
		log.Info("http server listening", slog.String("addr", cfg.HTTP.Addr))
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErrors <- err
		}
	}()

	select {
	case err := <-serverErrors:
		return fmt.Errorf("http server: %w", err)

	case <-ctx.Done():
		log.Info("shutdown signal received; draining",
			slog.Duration("grace", cfg.HTTP.ShutdownTimeout))

		// A fresh context: the signal context is already cancelled, and Shutdown needs
		// a live one to wait on in-flight requests.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.HTTP.ShutdownTimeout)
		defer cancel()

		if metricsServer != nil {
			_ = metricsServer.Shutdown(shutdownCtx)
		}
		if err := server.Shutdown(shutdownCtx); err != nil {
			// Close after a failed drain: in-flight requests are abandoned, but the
			// process must not hang past its termination grace period or the
			// orchestrator kills it with SIGKILL mid-write.
			_ = server.Close()
			return fmt.Errorf("graceful shutdown failed after %s: %w", cfg.HTTP.ShutdownTimeout, err)
		}
		log.Info("shutdown complete")
		return nil
	}
}

// buildCipher constructs the column cipher.
//
// In development an all-zero key is used when none is configured, so the platform runs
// with no setup. Config validation makes this impossible in production, where it would
// be equivalent to storing phone numbers in plaintext.
func buildCipher(cfg *config.Config, log *slog.Logger) (dataenc.Cipher, error) {
	if len(cfg.Crypto.DataKeys) == 0 {
		if cfg.Env.IsProduction() {
			return nil, errors.New("DATA_ENCRYPTION_KEYS is required")
		}
		log.Warn("no DATA_ENCRYPTION_KEYS configured; using a fixed development key — encrypted columns offer no protection")
		return dataenc.New([]dataenc.Key{{ID: "dev", Material: make([]byte, dataenc.KeyLength)}})
	}

	keys, err := dataenc.ParseKeys(cfg.Crypto.DataKeys)
	if err != nil {
		return nil, fmt.Errorf("data encryption keys: %w", err)
	}
	cipher, err := dataenc.New(keys)
	if err != nil {
		return nil, fmt.Errorf("data encryption keys: %w", err)
	}
	log.Info("column encryption ready",
		slog.String("primary_key_id", cipher.PrimaryKeyID()),
		slog.Int("keyring_size", len(keys)),
	)
	return cipher, nil
}

// startMetricsServer exposes Prometheus metrics on a separate port.
//
// Separate from the API listener on purpose: metrics must not be reachable through the
// public ingress, and binding them elsewhere makes that a network policy rather than a
// routing rule somebody has to remember.
func startMetricsServer(cfg *config.Config, log *slog.Logger) *http.Server {
	if !cfg.Telemetry.Enabled || cfg.Telemetry.MetricsAddr == "" {
		return nil
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := &http.Server{
		Addr:              cfg.Telemetry.MetricsAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Info("metrics server listening", slog.String("addr", cfg.Telemetry.MetricsAddr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// A metrics listener that cannot bind must not stop the API from serving.
			log.Error("metrics server stopped", slog.Any("error", err))
		}
	}()

	return srv
}

// buildTailoringService constructs the CV tailoring service, or nil when AI is off.
//
// Mirrors the worker's buildExtractor: the provider is pinged at boot rather than on
// the first request, because an API that starts healthy and then fails every tailoring
// attempt is far harder to diagnose than one that says so at startup.
func buildTailoringService(ctx context.Context, cfg *config.Config, db *postgres.DB, log *slog.Logger) (*cv.TailoringService, error) {
	if !cfg.AI.Enabled {
		if cfg.Env.IsProduction() {
			return nil, errors.New("AI_ENABLED must be true in production")
		}
		return nil, nil
	}

	provider := ai.NewAnthropicProvider(ai.AnthropicConfig{
		APIKey:     cfg.AI.APIKey,
		BaseURL:    cfg.AI.BaseURL,
		MaxRetries: cfg.AI.MaxRetries,
	})

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

	log.Info("CV tailoring enabled", slog.String("provider", provider.Name()))

	return cv.NewTailoringService(
		cv.NewPostgresRepository(db),
		cv.NewAITailorer(client, cv.TailorerConfig{}),
		log,
	), nil
}

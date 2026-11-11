package httpx

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/config"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/cors"
)

// HealthChecker reports one dependency's health for the readiness probe.
type HealthChecker interface {
	Name() string
	Check(ctx context.Context) error
}

// RouterDeps are the router's collaborators.
type RouterDeps struct {
	Config  *config.Config
	Logger  *slog.Logger
	Limiter Limiter
	Health  []HealthChecker
	// Mount registers the domain routes under /v1.
	Mount func(r chi.Router)
}

// NewRouter builds the HTTP handler with the full middleware stack.
//
// Middleware order is deliberate and load-bearing:
// request id first so everything downstream can log it; recovery immediately after so
// a panic anywhere below becomes a logged 500 with a request id; headers and CORS
// before anything that can reject; rate limiting before authentication so an
// unauthenticated flood is cheap to refuse; the access log last so it observes the
// real status.
func NewRouter(d RouterDeps) http.Handler {
	cfg := d.Config
	r := chi.NewRouter()

	r.Use(RequestID)
	r.Use(Recoverer)
	r.Use(SecurityHeaders)
	r.Use(HSTS(cfg.Env.IsProduction()))
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins: cfg.HTTP.CORSOrigins,
		AllowedMethods: []string{http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete, http.MethodOptions},
		AllowedHeaders: []string{"Accept", "Authorization", "Content-Type", HeaderRequestID, HeaderCorrelationID},
		ExposedHeaders: []string{HeaderRequestID},
		// Required for the refresh cookie to be sent from the frontend origin.
		AllowCredentials: true,
		MaxAge:           300,
	}))
	r.Use(AccessLog)
	r.Use(MaxBodyBytes(cfg.HTTP.MaxBodyBytes))

	// Probes are mounted outside the rate limiter: throttling a liveness probe gets
	// healthy pods restarted during exactly the traffic spike that caused it.
	r.Get("/healthz", healthzHandler)
	r.Get("/readyz", readyzHandler(d.Health, d.Logger))
	r.Get("/startupz", healthzHandler)

	r.Route("/v1", func(r chi.Router) {
		if d.Limiter != nil {
			// General API traffic fails open: a Redis outage must not take the whole
			// API down with it.
			r.Use(RateLimit(RateLimitConfig{
				Limiter:    d.Limiter,
				Limit:      300,
				Window:     time.Minute,
				KeyPrefix:  "api",
				TrustProxy: cfg.HTTP.TrustedProxy,
				FailOpen:   true,
			}))
		}
		r.Use(Timeout(cfg.HTTP.WriteTimeout - time.Second))

		if d.Mount != nil {
			d.Mount(r)
		}
	})

	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		JSON(w, req, http.StatusNotFound, map[string]string{"message": "no such endpoint"})
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		JSON(w, req, http.StatusMethodNotAllowed, map[string]string{"message": "method not allowed for this endpoint"})
	})

	return r
}

// AuthRateLimit is the stricter limiter for credential endpoints.
//
// It fails CLOSED, unlike the general API limiter: without a working limiter these
// endpoints are an open brute-force target, and refusing sign-ins for the duration of a
// Redis outage is the lesser harm.
func AuthRateLimit(limiter Limiter, trustProxy bool) func(http.Handler) http.Handler {
	return RateLimit(RateLimitConfig{
		Limiter:    limiter,
		Limit:      20,
		Window:     5 * time.Minute,
		KeyPrefix:  "auth",
		TrustProxy: trustProxy,
		FailOpen:   false,
	})
}

// healthzHandler answers liveness. It checks nothing: a liveness probe that depends on
// Postgres restarts every pod during a database blip, turning a degradation into an
// outage.
func healthzHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// readyzHandler answers readiness by checking every dependency.
//
// This one does gate traffic: a pod that cannot reach Postgres should be removed from
// the load balancer, which is exactly what a failing readiness probe achieves.
func readyzHandler(checkers []HealthChecker, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()

		results := make(map[string]string, len(checkers))
		healthy := true

		for _, c := range checkers {
			if err := c.Check(ctx); err != nil {
				healthy = false
				results[c.Name()] = "unavailable"
				log.WarnContext(ctx, "readiness check failed",
					slog.String("dependency", c.Name()), slog.Any("error", err))
				continue
			}
			results[c.Name()] = "ok"
		}

		status := http.StatusOK
		overall := "ready"
		if !healthy {
			status = http.StatusServiceUnavailable
			overall = "not_ready"
		}

		JSON(w, r, status, map[string]any{
			"status":       overall,
			"dependencies": results,
		})
	}
}

// CheckFunc adapts a function to HealthChecker.
type CheckFunc struct {
	DependencyName string
	Fn             func(ctx context.Context) error
}

// Name returns the dependency name.
func (c CheckFunc) Name() string { return c.DependencyName }

// Check runs the check.
func (c CheckFunc) Check(ctx context.Context) error { return c.Fn(ctx) }

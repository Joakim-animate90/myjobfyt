// Package sources wires the concrete job-source adapters into a registry.
//
// A separate package from jobs because each adapter imports jobs for the Adapter
// contract, so the package that assembles them cannot be jobs itself. Keeping the
// assembly here also means adding a source touches one file that already knows about
// every other, rather than being scattered through a binary's startup.
package sources

import (
	"log/slog"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/config"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/jobs"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/jobs/adzuna"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/jobs/remotefeeds"
)

// BuildRegistry constructs the adapters this deployment has credentials for.
//
// Each source is independently optional. An unconfigured source is reported at startup
// and left unregistered rather than failing the process: a deployment should run with
// whichever sources it has keys for, and "job discovery found nothing" is a far harder
// symptom to diagnose than a startup line saying which adapters are missing.
func BuildRegistry(cfg *config.Config, log *slog.Logger) (*jobs.Registry, error) {
	registry := jobs.NewRegistry()

	if cfg.JobSource.Adzuna.Configured() {
		adapter, err := adzuna.New(adzuna.Config{
			AppID:   cfg.JobSource.Adzuna.AppID,
			AppKey:  cfg.JobSource.Adzuna.AppKey,
			BaseURL: cfg.JobSource.Adzuna.BaseURL,
			Timeout: cfg.JobSource.Adzuna.Timeout,
			// The adapter owns its rate limit, so a scheduling bug cannot cause abuse.
			Limiter: jobs.NewTokenLimiter(cfg.JobSource.Adzuna.RequestsPerMinute),
		})
		if err != nil {
			return nil, err
		}
		if err := registry.Register(adapter); err != nil {
			return nil, err
		}
		log.Info("job source adapter registered",
			slog.String("source", adzuna.Slug),
			slog.Int("rate_per_min", cfg.JobSource.Adzuna.RequestsPerMinute),
		)
	} else {
		log.Warn("job source adapter not configured; it will not be used",
			slog.String("source", adzuna.Slug),
			slog.String("hint", "set ADZUNA_APP_ID and ADZUNA_APP_KEY"),
		)
	}

	// The free feeds need no credentials, so they are always registered. Whether they
	// are used is the database's decision — see job_sources.state — and whether they
	// may be used at all is a legal-basis question the executor checks.
	for _, adapter := range []jobs.Adapter{
		remotefeeds.NewRemotive(remotefeeds.Config{}),
		remotefeeds.NewRemoteOK(remotefeeds.Config{}),
		remotefeeds.NewArbeitnow(remotefeeds.Config{}),
	} {
		if err := registry.Register(adapter); err != nil {
			return nil, err
		}
	}

	log.Info("job source registry built", slog.Any("adapters", registry.Slugs()))
	return registry, nil
}

// ReportUnboundSources logs enabled sources this binary cannot talk to.
//
// Worth a loud line: it means the database is configured to use a source with no adapter
// compiled in, which otherwise shows up only as a source that silently returns nothing.
func ReportUnboundSources(unbound []string, log *slog.Logger) {
	for _, slug := range unbound {
		log.Error("a job source is enabled in the database but has no adapter in this build",
			slog.String("source", slug))
	}
}

# MyJobFyt

An intelligent job-search and application-automation platform. A candidate uploads a CV,
describes the roles they want, and the platform continuously discovers matching postings,
evaluates each one against their CV with an explainable score, prepares tailored
application materials, and submits — only ever with recorded candidate authorisation.

Built from a reverse-engineering study of LoopCV's publicly observable behaviour
(see [`docs/PRODUCT_ANALYSIS.md`](docs/PRODUCT_ANALYSIS.md)), with an original
implementation and a deliberately different stance on several points where the observed
product is weak.

## Status

| Area | State |
|---|---|
| Phase One — product analysis, feature inventory, journeys, workflows, domain model, integrations | complete |
| Architecture and event catalog | complete |
| Database schema (48 tables, 7 migrations) | complete, verified up/down against PostgreSQL 16 |
| Platform foundation — config, errors, HTTP, logging, Postgres, Redis, JetStream | complete |
| Identity — registration, sessions, JWKS, profiles | complete, running and verified end to end |
| Entitlements — plans, quotas, feature gating | complete |
| CV upload, library, antivirus, text extraction | complete, pipeline verified end to end |
| AI layer — provider abstraction, structured output, injection boundary | complete |
| CV structured extraction — schema, evidence verification, persistence | complete (never yet run against the real provider) |
| CV skill normalisation — curated taxonomy, exact matching | complete |
| CV embeddings, tailoring, checker | not started |
| Job acquisition, matching, applications, outreach, analytics, admin | not started |
| Frontend | not started |
| Kubernetes, Helm, FluxCD, CI/CD | not started |

537 tests passing, plus integration tests against live Postgres and MinIO.

## What makes this different

The reverse-engineering study surfaced several things worth doing differently, documented
in `docs/PRODUCT_ANALYSIS.md` §6:

- **Loops can be paused.** LoopCV offers only delete-or-keep-applying.
- **Match scores are explainable.** Per-dimension sub-scores and human-readable reasons on
  every match, instead of an opaque Low/Middle/High knob.
- **Rejections are retained with reasons**, so "why am I getting no matches?" is a
  self-service screen rather than a support ticket.
- **Every submission requires recorded authorisation** — per-application, or an explicitly
  bounded standing authorisation with a daily cap and expiry. This is enforced by a database
  constraint, not just a service check.
- **Prepared artefacts are frozen once authorised**, so what the candidate approved is
  byte-for-byte what gets sent.
- **CV tailoring never invents anything.** A containment validator rejects any output that
  introduces an employer, title, date, certification or skill absent from the source CV.
- **Sensitive screening questions are never auto-answered** on any tier.
- **Skill matching is exact, not fuzzy.** Measured: pg_trgm scores `scala`/`scale`
  (wrong) above `kotlin`/`koltin` (right), so no threshold is safe. A curated alias list
  plus a triage queue of unmatched labels does the job without ever conflating Java
  with JavaScript.
- **Authenticated job boards are reached only through the candidate's own browser session.**
  No CAPTCHA solving, no anti-bot circumvention, no credential relay.

## Stack

**Backend** — Go 1.26, chi, pgx/v5, PostgreSQL 16, Redis 7, NATS JetStream, goose
migrations, `log/slog`, OpenTelemetry, Prometheus.

**Frontend** — Next.js, TypeScript, Tailwind, shadcn/ui, React Query, Zod.

**Infrastructure** — Docker, Docker Compose, Kubernetes, Helm, FluxCD, Terraform,
GitHub Actions, Prometheus, Grafana, Loki.

## Getting started

Requires Go 1.26+, Docker and Docker Compose.

```bash
make env         # create .env from .env.example
make dev         # start dependencies and apply migrations
make run-api     # start the API on :18080
make run-worker  # in another shell: start the CV pipeline worker
```

Then, in another shell:

```bash
curl -s localhost:18080/readyz | jq
```

**Host ports are the service defaults plus 10000** (Postgres 15432, Redis 16379,
NATS 14222, MinIO 19000, Mailpit 11025/18025, API 18080). Development machines commonly
already have a Postgres on 5432 from another project, and silently connecting to the
wrong database is a bad afternoon.

| Service | URL |
|---|---|
| API | http://localhost:18080 |
| Metrics | http://localhost:19090/metrics |
| Mailpit (catches all outbound mail) | http://localhost:18025 |
| MinIO console | http://localhost:19001 |
| NATS monitoring | http://localhost:18222 |

### Trying the auth flow

```bash
# register
curl -s -X POST localhost:18080/v1/auth/register \
  -H 'Content-Type: application/json' \
  -d '{"email":"you@example.com","password":"correct-horse-battery-staple",
       "full_name":"Your Name","accepted_terms":true}'

# the verification link is waiting in Mailpit at http://localhost:18025
curl -s -X POST localhost:18080/v1/auth/verify-email \
  -H 'Content-Type: application/json' -d '{"token":"<from the email>"}'

# sign in; the refresh token arrives as an httpOnly cookie
curl -s -c jar.txt -X POST localhost:18080/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"you@example.com","password":"correct-horse-battery-staple"}'
```

### Trying the CV pipeline

With `make run-worker` running in another shell:

```bash
AT=<access_token from the login response>

# upload a CV
curl -s -X POST localhost:18080/v1/cvs \
  -H "Authorization: Bearer $AT" \
  -F "file=@/path/to/your-cv.pdf;type=application/pdf" \
  -F "label=My CV"

# poll it: pending -> scanning -> extracting -> parsed, usually within a second
curl -s localhost:18080/v1/cvs -H "Authorization: Bearer $AT" | jq '.data.cvs[0].latest_version'
```

Antivirus is disabled by default in development — the worker warns loudly on every
upload, and production refuses to start without `ANTIVIRUS_ADDR`. To run the real
scanner: `docker compose --profile full up -d clamav` and set
`ANTIVIRUS_ADDR=localhost:13310` (it takes a few minutes to download signatures on
first start).

### Useful targets

```bash
make check             # fmt, vet, unit tests, migration validation — run before committing
make test              # unit tests
make migrate-status    # which migrations have been applied
make migrate-reset     # roll everything back and re-apply (development only)
make psql              # psql shell on the development database
make keys              # generate a development RS256 keypair
make logs              # tail dependency logs
make clean             # stop the stack and delete its volumes
```

Without `AUTH_JWT_PRIVATE_KEY_PEM` the API generates an ephemeral signing key, so every
restart ends all sessions. Run `make keys` and paste the output into `.env` to avoid that.
Production refuses to start without a configured key.

## Documentation

| Document | What it covers |
|---|---|
| [`docs/PRODUCT_ANALYSIS.md`](docs/PRODUCT_ANALYSIS.md) | What was observed, and the design decisions it drove |
| [`docs/FEATURE_INVENTORY.md`](docs/FEATURE_INVENTORY.md) | 40 features across 9 domains, fully specified |
| [`docs/USER_JOURNEYS.md`](docs/USER_JOURNEYS.md) | 8 journeys with the sync/async split made explicit |
| [`docs/SYSTEM_WORKFLOWS.md`](docs/SYSTEM_WORKFLOWS.md) | 8 pipelines with idempotency keys, retries and dead-lettering |
| [`docs/DOMAIN_MODEL.md`](docs/DOMAIN_MODEL.md) | Bounded contexts, aggregates and invariants |
| [`docs/INTEGRATION_ANALYSIS.md`](docs/INTEGRATION_ANALYSIS.md) | Every job board classified by lawful access |
| [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) | Process topology, layering, reliability, security boundaries |
| [`docs/EVENT_CATALOG.md`](docs/EVENT_CATALOG.md) | Streams, events, consumers, versioning rules |
| [`CLAUDE.md`](CLAUDE.md) | Engineering conventions and hard product rules |

## Repository layout

```
backend/
  cmd/{api,worker,scheduler,migrate}   process entry points
  internal/
    config/        typed env configuration, validated at boot
    apperr/        error taxonomy; the single source of HTTP status mapping
    httpx/         router, middleware, binding, response envelope
    platform/      postgres, redisx, events, dataenc, mailer, telemetry
    domain/        identity, and the domains to come
  migrations/      goose SQL, embedded into the binary
frontend/          Next.js application
deploy/            docker, helm, flux, terraform, scripts
docs/              analysis, architecture and reference
```

## Licence

Not yet determined.

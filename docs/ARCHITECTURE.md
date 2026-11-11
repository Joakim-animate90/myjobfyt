# Architecture

## 1. Shape: a modular monolith with extractable seams

One deployable Go binary serving HTTP, plus a second binary running JetStream consumers,
plus a migration binary. Domains are isolated by package boundary and communicate in-process
through interfaces, or asynchronously through JetStream events.

This is deliberate. The pipeline has genuinely different scaling profiles — discovery is
I/O-bound against rate-limited third parties, parsing and matching are CPU- and
AI-cost-bound, submission is latency-tolerant but correctness-critical. That is an argument
for separating *workloads*, which separate worker deployments already achieve, not for
separating *services*. Splitting into microservices now would buy distributed transactions,
cross-service migrations, and a tenfold increase in failure modes, in exchange for nothing
we cannot get from a `--consumers` flag.

The seams are placed so extraction is mechanical when a real justification appears:
every domain exposes an interface, nothing reaches into another domain's tables, and all
cross-domain workflow already travels over JetStream.

```
                       ┌──────────────┐
                       │  Next.js     │  SSR + React Query
                       └──────┬───────┘
                              │ HTTPS / REST (OpenAPI)
                       ┌──────▼───────┐
                       │   Ingress    │  TLS, rate limit, WAF
                       └──────┬───────┘
         ┌────────────────────┼────────────────────┐
         │                    │                    │
   ┌─────▼─────┐       ┌──────▼──────┐      ┌──────▼──────┐
   │  api      │       │  worker     │      │  scheduler  │
   │ (N pods)  │       │ (M pods,    │      │ (1 pod,     │
   │           │       │  per-group) │      │  leader-el) │
   └─────┬─────┘       └──────┬──────┘      └──────┬──────┘
         │                    │                    │
   ┌─────┴────────────────────┴────────────────────┴─────┐
   │  Postgres 16   Redis 7   NATS JetStream   S3/MinIO  │
   └──────────────────────────────────────────────────────┘
                              │
                 ┌────────────┴────────────┐
                 │ AI provider · job APIs  │
                 │ SMTP · Stripe · geocode │
                 └─────────────────────────┘
```

## 2. Process topology

| Binary | Responsibility | Scaling |
|---|---|---|
| `api` | REST, auth, sync reads/writes, publishes events | Horizontal, stateless |
| `worker` | JetStream consumers, selected by `--groups=cv,jobs,matching,applications,outreach` | Horizontal per group; groups isolate blast radius and cost |
| `scheduler` | Cron planning: discovery, liveness, rollups, reapers | Single active instance via a Redis leader lock |
| `migrate` | goose migrations | Kubernetes Job / init container |

Splitting workers by group means an AI-provider outage stalls `cv` consumers while
`applications` keeps submitting, and a discovery backlog cannot starve submission.

## 3. Package layout

```
backend/
  cmd/{api,worker,scheduler,migrate}/main.go
  internal/
    config/              env-driven typed config, validated at boot
    apperr/              domain error taxonomy (the single source of HTTP mapping)
    httpx/               router, middleware, request binding, response envelope, error mapping
    platform/
      postgres/          pgxpool, tx manager, health
      redis/             client, locks, rate limiters, counters
      events/            JetStream publisher, consumer runtime, idempotency store
      storage/           S3-compatible object storage
      telemetry/         slog + OTel tracing/metrics, redaction
      mailer/            SMTP abstraction
      ai/                provider-agnostic LLM + embeddings client
    domain/
      identity/ cv/ jobs/ matching/ loops/ applications/ outreach/ analytics/ billing/ admin/
        model.go service.go repository.go handler.go *_test.go
    worker/              consumer registrations per group
  migrations/            goose SQL
  api/openapi.yaml
```

**Layering, enforced by review and by an import-boundary test:**
`handler → service → repository → database`. A handler never contains business logic and
never touches the database. A repository never contains business logic. A domain never
imports another domain's `repository`; it depends on the other domain's *interface*, defined
in the consumer package, so dependencies point inward.

## 4. Request lifecycle

```
request
  → request_id (generated or propagated) + OTel span
  → panic recovery
  → CORS, security headers
  → rate limit (Redis token bucket: per-IP anonymous, per-user authenticated)
  → JWT validation → principal in context
  → route
  → bind + validate (struct tags + explicit validation)
  → handler → service (business rules, entitlement checks, transaction boundary)
      → repository (user_id-scoped SQL)
      → publish events (after commit, never inside the transaction)
  → response envelope
  → structured access log (redacted) + metrics
```

Events are published **after** commit. Publishing inside a transaction risks emitting an event
for work that then rolls back; the cost is a small at-least-once window, which the consumers'
idempotency already handles.

## 5. Transaction management

A `TxManager` passes a transaction through `context`. Repositories transparently use the
context's transaction if present, otherwise the pool. Services declare boundaries:

```go
err := s.tx.InTx(ctx, func(ctx context.Context) error {
    if err := s.users.Create(ctx, u); err != nil { return err }
    return s.profiles.Create(ctx, p)
})
if err != nil { return err }
s.events.Publish(ctx, events.CandidateCreated{...})   // after commit
```

No nested transactions; a nested `InTx` joins the existing one. No business logic executes
inside a transaction that performs network I/O to a third party.

## 6. Error handling

One taxonomy in `apperr`, mapped to HTTP in exactly one place:

| Kind | HTTP | Body |
|---|---|---|
| `Validation` | 422 | field-level details |
| `Unauthenticated` | 401 | generic |
| `Forbidden` | 403 | generic |
| `NotFound` | 404 | resource type |
| `Conflict` | 409 | reason |
| `QuotaExceeded` | 429 | quota key, limit, reset time |
| `RateLimited` | 429 | `Retry-After` |
| `Dependency` | 502/503 | generic + `request_id` |
| `Internal` | 500 | generic + `request_id` only |

Internal messages never reach the client. Every error response carries the `request_id`, which
is also the key into logs and traces — so a user-reported error is one query away from its trace.

## 7. Event backbone

Streams, subjects, and consumer configuration live in `EVENT_CATALOG.md`. The invariants:

- **Subjects:** `myjobfyt.<domain>.<event>` — e.g. `myjobfyt.cv.uploaded`.
- **Streams:** one per domain, file-backed, work-queue or interest retention, explicit max age.
- **Consumers:** durable, explicit ack, `AckWait` sized above the handler timeout,
  `MaxDeliver` set, overflow to a per-stream dead-letter subject.
- **Envelope:** `event_id`, `type`, `version`, `occurred_at`, `correlation_id`,
  `causation_id`, `user_id`, `payload`.
- **Idempotency:** every handler computes a deterministic key and claims it in
  `processed_messages` before performing side effects. A key found `in_flight` on redelivery
  means the prior outcome is unknown and the handler must report `outcome_unknown` rather than
  repeating a side effect.
- **Versioning:** payloads are additive-only. A breaking change is a new `version` with both
  handled during the transition.
- **Observability:** `events_published_total`, `events_processed_total{outcome}`,
  `event_processing_duration_seconds`, `consumer_pending`, `dead_letter_depth` (alerts > 0).

## 8. Authentication & authorisation

- Access token: RS256 JWT, 15 min, claims `sub`, `role`, `plan`, `jti`, `exp`. Keys rotatable,
  published via JWKS so the frontend and extension can validate without a shared secret.
- Refresh token: opaque 256-bit, stored hashed with a `family_id`, 30 days, httpOnly cookie,
  single-use with rotation. Reuse revokes the family — the standard theft-detection mechanism.
- Extension token: short-lived, audience-scoped to the extension API surface only.
- Authorisation: default-deny policy middleware **plus** mandatory `user_id` scoping in the
  repository layer. Two independent layers, because a single forgotten handler check should not
  be a data breach.

## 9. Observability

- **Logs:** `log/slog` JSON to stdout → Promtail → Loki. Always `request_id`, `user_id`,
  `trace_id`. A redaction layer in the logging package strips CV text, passwords, tokens,
  SMTP credentials, and AI prompt bodies. Tests assert the redaction holds.
- **Metrics:** Prometheus. RED for HTTP, per-consumer event metrics, AI token and cost
  counters, per-source discovery health, submission outcomes, quota usage.
- **Traces:** OpenTelemetry, propagated into event envelopes so an async chain is one trace —
  this is what makes the admin's end-to-end workflow view (I2) possible at all.
- **Dashboards & alerts:** Grafana; alerts on dead-letter depth, consumer lag, submission
  failure rate, AI error rate, p99 latency, DB saturation, quota-rejection spikes.

## 10. Reliability

| Concern | Mechanism |
|---|---|
| Graceful shutdown | SIGTERM → stop accepting → drain in-flight (30s) → drain consumers → close pools |
| Health | `/healthz` (liveness, no dependencies) · `/readyz` (DB, Redis, NATS) · `/startupz` |
| Backpressure | Bounded worker concurrency per group; JetStream `MaxAckPending` |
| Third-party failure | Per-dependency circuit breaker + timeout + bounded retry with jitter |
| Partial degradation | Embeddings down → lexical matching, flagged for rescore. AI down → deterministic CV checks only, explicitly labelled. Redis down → JWT auth still works; quotas fail to a conservative Postgres-backed cap |
| Poison messages | `MaxDeliver` → dead-letter → admin requeue after fix |
| Hot-partition abuse | Per-user and per-IP rate limits; per-destination-domain send limits |

## 11. Data

Postgres is the system of record. Redis holds only reconstructible state: cache, locks, rate
limiters, live counters, Loop pause kill-flags. CVs live in object storage; Postgres stores keys.
Downloads are presigned and short-lived, so the application is never on the bytes path.

## 12. Security boundaries

1. **Upload boundary.** Magic-byte verification, size cap enforced during streaming, AV scan,
   opaque per-tenant keys, no execution, never served from the app origin.
2. **Prompt-injection boundary.** CV text, job descriptions, screening questions, and inbound
   email are untrusted. They are passed as delimited data, never in an instruction position;
   all AI output is schema-validated before use. CV tailoring additionally passes a factual
   containment check against the source CV.
3. **Tenant boundary.** `user_id` scoping in the repository layer, asserted by tests.
4. **Submission boundary.** No send without a valid, recorded authorisation. Enforced at the
   repository level and re-checked immediately before dispatch.
5. **Credential boundary.** The server never holds a candidate's job-board credentials or
   session cookies. Authenticated boards are reached only through the candidate's own browser.
6. **Secret boundary.** SOPS/age in Git, External Secrets in-cluster, nothing plaintext.

## 13. Deployment

Docker Compose for local development with the full dependency set. Kubernetes via Helm for
staging and production, reconciled by FluxCD from Git. One chart, three value files. Initial
target is a single-node VPS cluster (k3s); the migration path to managed Kubernetes is a
values change plus moving Postgres to a managed instance, because nothing in the application
assumes node locality.

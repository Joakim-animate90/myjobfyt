# Technical Debt

Debt knowingly taken, with the reason and the condition that should trigger paying it
down. Debt that is not written down becomes a surprise; debt without a trigger becomes
permanent.

Items marked **deliberate** are sound choices for the current stage that will stop being
sound at a stated threshold. Items marked **shortcut** should be paid down sooner.

---

## TD-01 — The common-password list is a 32-entry floor, not a corpus
**Kind:** shortcut · **Where:** `internal/domain/identity/password.go`

A hardcoded map of the passwords that dominate breach corpora. It catches `password1234`
and `qwerty12345` and nothing else.

**Why.** A real check needs either a network call per registration (Have I Been Pwned's
k-anonymity range API) or a multi-megabyte local bloom filter. Neither is justified
before there are users, and the 12-character minimum already excludes most of the
practical attack surface.

**Pay down when:** before public launch. Tracked in `MJF-087`.

**Risk if ignored:** a user picks a long passphrase that nonetheless appears in a breach
corpus, and credential stuffing succeeds against them.

---

## TD-02 — Embeddings stored as `real[]` rather than `pgvector`
**Kind:** deliberate · **Where:** `migrations/00002_cv.sql`

Vectors live in a `real[]` column with a dimension check, in their own table, and
similarity is computed in Go.

**Why.** `pgvector` is not available in the stock `postgres:16-alpine` image, and
requiring a custom image or an extension install would complicate every developer's
setup for a feature that works correctly without it at current scale. The separate table
exists precisely so the column type can change without touching `cv_versions`.

**Pay down when:** similarity search becomes a query rather than an in-process
computation — roughly when matching needs nearest-neighbour search over more than a few
thousand vectors per request.

**Risk if ignored:** matching latency grows linearly with corpus size.

---

## TD-03 — No OpenAPI specification yet
**Kind:** shortcut · **Where:** `backend/api/` is empty

The API has no machine-readable contract. The frontend will be written against the
implementation.

**Why.** The endpoints were still moving; a specification written alongside a changing
API would have been stale on arrival.

**Pay down when:** before the frontend's API client is written, so it can be generated
rather than hand-maintained. Tracked in `MJF-086`.

**Risk if ignored:** frontend and backend drift, and the extension has nothing reliable
to build against. The mitigation that matters is the contract *test* — a specification
nobody verifies is worse than none, because it is believed.

---

## TD-04 — Integration tests exist but depend on a hand-started stack
**Kind:** shortcut · **Where:** `//go:build integration` tests across `internal/`

There are now 760 integration cases running against real Postgres, NATS and MinIO,
including the CV pipeline end to end, the idempotency store's claim and concurrency
semantics, and the embedding storage round trip. They are skipped unless `POSTGRES_DSN`,
`NATS_URL` and `STORAGE_ENDPOINT` are set, so they pass vacuously wherever the stack is
not running — which includes CI today.

**Why.** The tests were worth having before the harness that runs them automatically;
writing them first is what surfaced the lost dead letters and the flattened traces. The
remaining work is a testcontainers harness so they bring up their own dependencies and
cannot be skipped into silence.

**Pay down immediately.** Still the highest-priority debt item, now narrowed from "no
integration tests" to "no automation and no guard against silent skipping". Tracked in
`MJF-081`, which should also fail the build if the integration suite reports zero tests
run.

**Risk if ignored:** the suite looks green while testing nothing. That is worse than an
absent suite, because an absent one does not give false assurance. The invariants
protecting candidates from unauthorised submissions and fabricated CVs are covered by
tests nobody is guaranteed to run.

---

## TD-05 — Development uses ephemeral signing keys and a zero encryption key
**Kind:** deliberate · **Where:** `cmd/api/main.go`

With no `AUTH_JWT_PRIVATE_KEY_PEM` the API generates a keypair at boot; with no
`DATA_ENCRYPTION_KEYS` it uses an all-zero AES key. Both log a prominent warning, and
configuration validation makes both impossible in production.

**Why.** `make dev` to a working platform with no setup is worth a great deal, and the
production guard means the convenience cannot escape development.

**Pay down when:** never, for development. The guard is the control.

**Risk if ignored:** none in production. In development, every restart ends all sessions,
which is confusing until you know — hence the warning and the `make keys` target.

---

## TD-06 — Chainguard MinIO image pinned to a rolling tag
**Kind:** shortcut, externally imposed · **Where:** `docker-compose.yml`

`chainguard/minio:latest` rather than a pinned release.

**Why.** Neither `minio/minio` on Docker Hub nor `quay.io/minio/minio` resolves through
this host's registry configuration. The Chainguard mirror publishes a rolling tag only.
The constraint is the environment's, not a choice.

**Pay down when:** CI, where the upstream registry is reachable, pins by digest. Also
reconsider for anyone else developing on a differently-configured machine.

**Risk if ignored:** a MinIO upgrade changes behaviour mid-development with no commit
explaining it.

---

## TD-07 — No frontend exists, so the API's ergonomics are unproven
**Kind:** deliberate · **Where:** `frontend/` is empty

The identity API is verified by curl, not by a real client.

**Why.** Backend-first was the right order given that the frontend is specified in detail
and the API shape was uncertain.

**Pay down when:** `MJF-050` and `MJF-051`. Expect to find response-shape problems that
curl does not reveal — the partial-update pointer semantics and the `missing_fields`
contract in particular.

**Risk if ignored:** an API that is correct but awkward, discovered late.

---

## TD-08 — `ClientIP` trusts `X-Forwarded-For` behind a single boolean
**Kind:** deliberate · **Where:** `internal/httpx/middleware.go`

`HTTP_TRUST_PROXY_HEADERS` is all-or-nothing. When true, the left-most forwarded entry is
taken as the client.

**Why.** Correct for the intended deployment: a single trusted ingress terminating TLS.
Proper handling needs a configurable trusted-proxy CIDR list and right-to-left walking,
which is only meaningful once the proxy topology is fixed.

**Pay down when:** a second proxy layer appears, such as a CDN in front of the ingress.
At that point the left-most entry becomes attacker-controlled again.

**Risk if ignored:** with the flag wrongly enabled, per-IP rate limiting can be defeated
by spoofing the header. The default is `false`, which fails safe.

---

## TD-09 — Entitlement quota values are chosen, not validated
**Kind:** deliberate · **Where:** `migrations/00007_seed_reference_data.sql`

Free tier at 5 applications and 1 loop per day, Pro at 25 and 3, and so on.

**Why.** LoopCV's public pricing page contradicts itself on its own free-tier limit (see
`PRODUCT_ANALYSIS.md` §7), so there was nothing reliable to copy. Choosing conservatively
and documenting the choice is better than guessing and presenting the guess as research.

**Pay down when:** real usage data exists. They are config, so changing them is a seed
update, not a migration.

**Risk if ignored:** limits that are too tight to convert, or too loose to be
economically viable, with AI spend as the sensitive term.

---

## TD-10 — No request-scoped AI cost ceiling — RESOLVED
**Kind:** shortcut · **Where:** `internal/platform/ai`
**Resolved:** 2026-11-11 in `MJF-004`. `Request.MaxTokenBudget` caps a single call
independently of the monthly quota, enforced before the provider is called, with a test
asserting an oversized document is refused without a call being made.

The `ai_tokens_month` quota is monthly. A single pathological request — a 200-page PDF,
or a retry loop — could consume a large share of a month's allowance in minutes.

**Why.** Not yet built; recording it now so it is designed in rather than retrofitted.

**Pay down with:** `MJF-004`. A per-request and per-operation token ceiling, independent
of the monthly quota.

**Risk if ignored:** one upload exhausts a candidate's month, or a retry loop produces an
unbounded bill.

---

## TD-11 — `applications.prepared_*` artefacts live in Postgres
**Kind:** deliberate · **Where:** `migrations/00005_applications.sql`

Rendered cover letters and email bodies are `text` columns.

**Why.** They must be immutable and transactionally consistent with the authorisation
that covers them, which object storage cannot provide. They are kilobytes, not megabytes.

**Pay down when:** the table's size becomes a backup or vacuum problem — plausibly at a
few million applications. The fix is a content-addressed store with the hash in Postgres,
preserving immutability.

**Risk if ignored:** table bloat and slower backups, long before any correctness issue.

---

## TD-12 — `cv.embedded` has no consumer, so degraded match scores are never upgraded
**Kind:** deliberate · **Where:** `internal/domain/cv/worker.go`

When the embedding provider is unavailable, matching falls back to the lexical path and
marks the score degraded. The CV is re-embedded later by the backfill sweep, which
publishes `cv.embedded` so that those scores can be recomputed. Nothing subscribes to
that event today.

**Why.** Rescoring is matching's job and matching does not exist yet (`MJF-031`).
Everything rescoring needs is in place — the vectors, the recorded method, the recovery
sweep and the event — but wiring a consumer before there is anything to rescore would
mean inventing the scoring model here, in the wrong domain.

**Pay down when:** `MJF-031` lands. It must subscribe to `cv.embedded` and recompute any
match whose score was recorded as degraded, rather than only scoring new matches.

**Risk if ignored:** a candidate whose CV happened to be processed during an embedding
outage keeps weaker match scores permanently, with nothing in the product indicating it.
This is invisible to the candidate and to operators, which is what makes it worth
recording rather than remembering.

---

## TD-13 — Stream retention changes require manual intervention
**Kind:** deliberate · **Where:** `internal/platform/events/streams.go`

JetStream cannot change a stream's retention policy in place. `EnsureStreams` detects a
mismatch and fails with the stream name and the command to run, rather than applying it.

**Why.** Applying it means deleting the stream, which discards every message not yet
acked by all of its consumers — for the CV stream, a candidate's upload mid-pipeline.
That is an operator's decision, made after draining the workers, not something a process
should do to itself on startup.

**Pay down when:** a retention change is needed in an environment where the manual step
is unacceptable. The fix is a documented drain-and-recreate runbook step, not automation
in `EnsureStreams`.

**Risk if ignored:** a deployment whose topology changed retention fails to start, with
an error that says exactly what to do. Loud and safe, which is the intent.

---

## Summary

| ID | Kind | Priority | Tracked by |
|---|---|---|---|
| TD-04 | shortcut | **immediate** | `MJF-081` |
| TD-03 | shortcut | high | `MJF-086` |
| ~~TD-10~~ | ~~shortcut~~ | resolved | `MJF-004` |
| TD-01 | shortcut | before launch | `MJF-087` |
| TD-06 | shortcut | low | `MJF-070` |
| TD-07 | deliberate | resolves naturally | `MJF-050` |
| TD-12 | deliberate | with `MJF-031` | `MJF-031` |
| TD-02, TD-08, TD-09, TD-11, TD-13 | deliberate | threshold-triggered | — |
| TD-05 | deliberate | never | — |

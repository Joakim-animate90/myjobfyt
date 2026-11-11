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

## TD-04 — Integration tests are not yet automated
**Kind:** shortcut · **Where:** no `//go:build integration` tests exist

Repository SQL, the database invariants and the migration round-trip were all verified by
hand against real Postgres during this work, and the results are recorded in
`ENGINEERING_AUDIT.md`. None of it is automated.

**Why.** The testcontainers harness is a substantial piece of work in itself, and the
schema was still settling.

**Pay down immediately.** This is the highest-priority debt item. Tracked in `MJF-081`.

**Risk if ignored:** the invariants protecting candidates from unauthorised submissions
and fabricated CVs are currently verified *once*, not continuously. A refactor could
remove a constraint and nothing would notice.

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

## TD-10 — No request-scoped AI cost ceiling
**Kind:** shortcut · **Where:** will be `internal/platform/ai`

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

## Summary

| ID | Kind | Priority | Tracked by |
|---|---|---|---|
| TD-04 | shortcut | **immediate** | `MJF-081` |
| TD-03 | shortcut | high | `MJF-086` |
| TD-10 | shortcut | high | `MJF-004` |
| TD-01 | shortcut | before launch | `MJF-087` |
| TD-06 | shortcut | low | `MJF-070` |
| TD-07 | deliberate | resolves naturally | `MJF-050` |
| TD-02, TD-08, TD-09, TD-11 | deliberate | threshold-triggered | — |
| TD-05 | deliberate | never | — |

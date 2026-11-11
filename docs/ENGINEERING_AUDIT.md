# Engineering Audit

**Date:** 2026-11-11
**Auditor role:** technical lead, pre-implementation assessment

## 1. Starting condition

The repository contained one commit and one file: an eleven-byte `README.md`. There was
no code, no schema, no dependency manifest, no tests, no CI and no infrastructure.

This matters for how the audit is framed. The directive asks for an inspection of
"existing architecture, directory structure, dependencies, database schema, API
contracts, authentication, authorization, business logic, frontend architecture, existing
integrations, tests, build processes, CI/CD, infrastructure, security weaknesses,
performance bottlenecks, missing functionality, technical debt." **None of those existed
to inspect.** Reporting findings against them would be fabrication.

What the audit can honestly do on a greenfield repository is a **gap analysis against the
target architecture**, which is what produced the 61-issue backlog in
[`ISSUES.md`](ISSUES.md). Every item there is a genuine gap between the current state and
a production-grade implementation, not a defect discovered in existing code.

## 2. Environment assessment

The toolchain was inspected and is adequate, with two gaps.

| Tool | Status |
|---|---|
| Go | 1.26.2 — current |
| Node | 24.15.0 — current |
| Docker | 29.4.2 with Compose v5.1.3 |
| kubectl | 1.36.0 with Kustomize 5.8.1 |
| Terraform | 1.17.1 |
| **Helm** | **not installed** — blocks `MJF-071` |
| psql / redis-cli | not installed; both reachable via `docker compose exec` |
| gh | authenticated as Joakim-animate90 with repo and workflow scopes |

**Finding E1 — Helm missing.** Must be installed before chart work begins. Recorded as a
prerequisite on `MJF-071`.

**Finding E2 — port collisions on the development machine.** Ports 5432, 6379, 9000,
9001, 1025, 8025, 3000 and 8080 were already bound by two other projects on this machine.
A development stack on default ports would have connected to another project's Postgres,
which is a quiet and occasionally destructive failure. **Resolved:** every host port in
`docker-compose.yml` is the service default plus 10000, documented in the README and in
the `verify-stack` skill.

**Finding E3 — registry constraints.** `minio/minio` and `minio/mc` do not resolve
through this host's registry configuration; only a Chainguard mirror is reachable.
**Resolved:** the compose file uses `chainguard/minio`, and bucket creation moved into the
application rather than an `mc`-based init container. The chart will pin by digest, where
the upstream registry is reachable. Recorded in the `verify-stack` skill so it is not
rediscovered.

## 3. What has been built since, and its assessed quality

| Subsystem | State | Assessment |
|---|---|---|
| Configuration | Typed, env-driven, validated at boot with stricter production rules | Sound. A process with invalid configuration fails to start rather than failing under load. |
| Error taxonomy | One `Kind` per failure class, mapped to HTTP in exactly one place | Sound. A handler cannot invent its own error semantics, and an internal message cannot leak from a 500. |
| HTTP layer | Envelope, strict binding, pagination clamping, full middleware stack | Sound. One defect found and fixed during verification — see D1. |
| Logging | Redaction enforced in the `slog` handler itself | Sound. A forgotten call site cannot cause a disclosure. Operational counters are explicitly allowlisted so redaction does not blind the pipeline metrics. |
| Postgres | Pool with session statement timeout, context-threaded transactions, typed constraint predicates | Sound. Nested `InTx` joins rather than nests, which avoids savepoint semantics quietly differing from the caller's expectation. |
| Redis | Lua-scripted limiter, counters and fencing-token locks | Sound. `INCR`/`EXPIRE` and `GET`/`DEL` cannot interleave, and pause flags fail closed on error. |
| Event backbone | Envelope with explicit W3C trace context, stream topology, consumer runtime | Sound, and the strongest part of the foundation. The in-flight ambiguity policy is the key design decision. One defect found and fixed — see D2. |
| Database schema | 48 tables, 7 migrations, invariants pushed into the schema | Sound. Verified up, down with zero residue, and up again. Every new invariant was watched rejecting a bad write. |
| Identity | Registration, sessions, JWKS, profiles | Sound. Verified end to end against the live stack. One defect found and fixed — see D3. |
| Everything else | Not built | 61 issues. |

## 4. Defects found and fixed during this work

These are real defects, each found by verification rather than by review, and each worth
recording because the class of mistake will recur.

**D1 — The strict credential rate limiter was applied to an entire domain.**
`cmd/api` mounted `AuthRateLimit` (20 requests per 5 minutes, fail-closed) around every
identity route. The consequence: a candidate browsing their own settings would be
throttled after twenty requests, and any client polling `/v1/auth/jwks` to validate
tokens would be locked out of verifying anything. Found by load-testing the endpoints
rather than by reading the code — 19 of 30 JWKS polls failed.
**Fixed** by scoping the limiter inside `Handler.RegisterRoutes` to the endpoints that
actually accept or mint credentials, and never to logout, since a throttled sign-out is
the opposite of a security control. Verified: 40/40 JWKS polls and 40/40 authenticated
reads now succeed while brute-force attempts still throttle at 20.

**D2 — `Nak()` ignores the consumer's backoff schedule.**
The consumer runtime configured an explicit JetStream `BackOff` schedule and then called
`msg.Nak()`, which is documented to trigger *instant* redelivery and ignore that
schedule. A failing dependency would therefore be hammered at full speed until
`MaxDeliver` was exhausted — the opposite of backoff, and exactly the behaviour that
turns a brief third-party blip into an outage.
**Fixed** by using `NakWithDelay` against an explicit schedule, with a test asserting the
delay is non-zero and monotonic.

**D3 — `NewRefreshToken` read the wall clock.**
The identity service takes an injectable clock precisely so that expiry and lockout
behaviour is testable, but refresh-token expiry was computed from `time.Now()` inside the
token constructor. The service's notion of "now" and the token's silently disagreed, and
the expired-token test passed for the wrong reason.
**Fixed** by making the issue time an explicit argument. Security behaviour should not
depend on a helper reaching for the clock behind the caller's back.

## 5. Security posture

Verified in place:

- Password storage: argon2id at 64 MiB / t=3 / p=2, with self-describing hashes enabling
  a cost increase without invalidating existing passwords.
- Token forgery: RS256 pinned; both `alg=none` and HMAC-signed-with-the-public-key
  forgeries are rejected, with tests.
- Audience isolation: an extension token is rejected against the main API.
- Refresh token theft: rotation families, with replay of a superseded token revoking the
  whole family. Verified live.
- Account enumeration: registration, sign-in and verification all give identical
  responses regardless of whether the account exists, with work burned on the miss path
  so timing does not leak either.
- Tenant isolation: `user_id` scoping in the repository layer, plus database triggers on
  the three tables where several ownership paths converge.
- Consent: an application cannot reach a sent state without an authorisation, and
  prepared artefacts are frozen once authorised. Both enforced by the database.
- Log redaction: verified against the running server — no password, phone number or
  token appears in any log line.

Not yet addressed, each tracked as an issue:

- `MJF-084` — no automated dependency, container, secret or SAST scanning.
- `MJF-087` — no threat model or security documentation.
- `MJF-001` — upload validation and antivirus not yet implemented; this will be the
  highest-risk input in the product.
- `MJF-011` — the factual-containment validator that prevents CV fabrication.
- `MJF-066` — GDPR export and erasure.
- The common-password list is a 32-entry floor, not a corpus. Production should check
  against a full set via a k-anonymity range query or a local bloom filter. Documented in
  the code and tracked in `MJF-087`.

## 6. Performance assessment

Nothing has been load-tested; `MJF-085` establishes the baseline. The known risks, each
already mitigated in design:

- **Match fan-out is quadratic** without a prefilter. `MJF-032` adds an inverted index.
- **Discovery cost scales with candidates** unless keyed by search specification rather
  than by candidate. Designed in from the start and specified in `MJF-024`.
- **A daily re-sighting storm** would re-match the entire corpus every day. Prevented by
  publishing `jobs.normalised` only when content actually changed.
- **AI cost is the dominant variable cost.** Token accounting and quota enforcement are
  in `MJF-004` and `MJF-065`; caching by content hash is in `MJF-008`.

## 7. Conclusion

The foundation is sound and verified rather than assumed: 254 unit tests, a schema proven
reversible, and an identity flow exercised end to end against real Postgres, Redis, NATS
and SMTP. Three genuine defects were found by that verification and fixed.

The honest summary of remaining risk is that **the product's hardest and most
safety-critical work is still ahead**: `MJF-011` (no fabricated CV content), `MJF-042`
(no submission without consent) and `MJF-043` (no duplicate submissions). The schema
already enforces the invariants those features must respect, which is the right order to
have done it in — but the features themselves are unwritten, and they are where this
product can do real harm to a real person's job search if built carelessly.

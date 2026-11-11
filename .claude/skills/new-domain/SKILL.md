---
name: new-domain
description: The checklist for adding a new backend domain to MyJobFyt end to end — migration, model, repository, service, handler, worker consumers, wiring, tests and verification. Use when starting work on cv, jobs, matching, applications, outreach, analytics, billing or admin.
---

# Adding a domain

Work in this order. Each step is verifiable before the next begins, which is what keeps a
large domain from becoming a single untestable change.

## 0. Read first

- `docs/DOMAIN_MODEL.md` — the intended entities and invariants for this domain
- `docs/FEATURE_INVENTORY.md` — the specified behaviour, including failure modes
- `docs/SYSTEM_WORKFLOWS.md` — the async pipeline, idempotency keys and retry policy
- `docs/EVENT_CATALOG.md` — the events this domain publishes and consumes
- `backend/internal/domain/identity/` — the reference implementation for every pattern

## 1. Migration

The tables already exist for every domain in `docs/DOMAIN_MODEL.md`, so usually there is
nothing to add. If there is, use the `migration-author` agent and push the invariants into
the schema. Verify with `make migrate-reset`.

## 2. `model.go`

Types, enums matching the database enums exactly, and pure domain logic with no I/O.
Anything computable from the struct alone belongs here, where it is trivially testable —
`Profile.Completeness()` and `Status.CanAuthenticate()` are the pattern.

Write the model tests now. They are fast and they pin the business rules before any
plumbing exists to obscure them.

## 3. `repository.go`

Declare the `Repository` interface first — the service depends on the interface, not the
implementation — then write the Postgres type.

Every query touching candidate data carries a `user_id` predicate **in the SQL**. Map
constraint violations to domain errors with `postgres.IsUniqueViolation(err, "name")` so
the service can distinguish "email taken" from any other collision.

Never select a ciphertext column on a read path that does not need to decrypt it.

## 4. `service.go`

Business logic, transaction boundaries, entitlement checks, events and audit.

- `ctx` first, `apperr` errors out.
- Publish events **after** commit.
- Audit and event failures log and continue; they never fail the operation.
- Inject the clock if any behaviour depends on time.
- Decide every degradation deliberately: fail open or closed, and write the test that
  pins the choice.

## 5. `fake_repo_test.go` and `service_test.go`

The fake must reproduce the behaviours the service actually relies on — case-insensitive
uniqueness, single-use tokens, atomic counter updates. A fake that gets those wrong tests
nothing useful.

Cover: the happy path, every validation rejection with its field and code, tenant
isolation, the degradation paths, and idempotency for anything with a side effect.

## 6. `handler.go`

Bind, delegate, shape. No business logic.

Use pointer fields on update requests so an omitted field differs from a cleared one.
Register routes through `RouteDeps`, and apply the strict credential limiter **only** to
endpoints that accept or mint credentials — mounting it domain-wide throttles ordinary
browsing and breaks public endpoints.

## 7. Worker consumers

Register in `internal/worker` under this domain's group, so its failures cannot stall
another domain's pipeline.

For each consumer, decide and write down: the idempotency key, `AckWait` (above the
handler's worst case), `MaxDeliver`, and the `InFlightPolicy`.

**`InFlightUnknown` for anything with an irreversible external side effect** — submitting
an application, sending an email. `InFlightRetry` only where redoing the work is harmless.
Getting this wrong means a candidate applies twice to the same job.

## 8. Wire it in `cmd/api/main.go`

Construct repository, service, handler; mount the routes; add any new dependency to the
readiness probe.

## 9. Verify for real

```bash
make check                     # fmt, vet, unit tests, migration validation
make up && make migrate
make run-api
```

Then exercise the new endpoints against the live stack with curl, following the
`smoke-auth` skill's pattern. Check the database for the rows you expect, and read the
log for warnings you did not expect. **A feature is not complete because it compiles or
because its unit tests pass.**

## 10. Update the docs

The status table in `README.md`, and anything in `docs/` the implementation taught you
was wrong. A document that no longer matches the code is worse than no document.

---
name: verify-stack
description: Bring up the MyJobFyt development stack, apply migrations, start the API and confirm every dependency is healthy. Use when starting work on this repository, after a machine restart, when something "isn't connecting", or before any end-to-end verification.
---

# Verify the development stack

Host ports are the service defaults **plus 10000**, because this machine already has a
Postgres on 5432 and a Redis on 6379 from other projects. Connecting to the wrong
database is the failure this offset exists to prevent — if a command seems to work
against an unexpected schema, check the port first.

## Bring it up

```bash
cd /home/kimongi/code/myjobfyt
make env            # creates .env from .env.example if absent
make up             # starts postgres, redis, nats, minio, mailpit and waits for health
make migrate        # applies the goose migrations
```

Confirm all five containers report healthy:

```bash
docker compose ps --format '{{.Service}}\t{{.State}}\t{{.Status}}'
```

## Start the API

Do **not** background it with `go run ... &` inside a compound command: the `&` has
lower precedence than `&&`, so the whole chain runs in a subshell and the pid is lost.
Build the binary and record the pid instead, so it can be stopped precisely.

```bash
cd backend && go build -o /tmp/myjobfyt-api ./cmd/api && cd ..
set -a; . ./.env; set +a
nohup /tmp/myjobfyt-api > /tmp/myjobfyt-api.log 2>&1 &
echo $! > /tmp/myjobfyt-api.pid
```

**Never stop it with `pkill -f "go run ./cmd/api"`.** That pattern matches the shell
running the command and kills the session. Use the pidfile:

```bash
kill -TERM "$(cat /tmp/myjobfyt-api.pid)" && rm -f /tmp/myjobfyt-api.pid
```

SIGTERM rather than SIGKILL, so the graceful-drain path is exercised.

## Confirm health

```bash
curl -s localhost:18080/healthz          # liveness: checks nothing, by design
curl -s localhost:18080/readyz | jq      # readiness: postgres, redis, nats
curl -s localhost:19090/metrics | head   # prometheus
```

`/readyz` must report `"status": "ready"` with all three dependencies `ok`. If one is
`unavailable`, the API is running correctly and the dependency is not — check
`docker compose ps` before looking at application code.

## Service endpoints

| Service | Where |
|---|---|
| API | http://localhost:18080 |
| Metrics | http://localhost:19090/metrics |
| Mailpit (all outbound mail lands here) | http://localhost:18025 |
| MinIO console | http://localhost:19001 |
| NATS monitoring | http://localhost:18222 |
| Postgres | `make psql` |

## If more than one API process is running

Two processes means the second failed to bind and the first is serving stale code, which
produces results that contradict the source you are reading. Check and clean up:

```bash
pgrep -af 'myjobfyt-api$'
```

## Known environment constraints on this machine

- `minio/minio` and `minio/mc` do not resolve through this host's registry; the compose
  file uses `chainguard/minio`. Bucket creation is therefore the application's job, not
  an init container's.
- Helm is not installed. Install it before working on the charts.

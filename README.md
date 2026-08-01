# Sentinel

Minimalistic system to track errors for your backend service.

```
sdk → ingestor-go (HTTP :8080) → NATS JetStream → processor-go → PostgreSQL
```

`dashboard-web` (SvelteKit + Auth.js + Drizzle) reads the same PostgreSQL database to render issues,
occurrences, organizations, and alerting configuration.

> [!IMPORTANT]
> Several features in `specs/` are marked "Completed", which records a merge event, not verified behavior.
> Before trusting any feature-complete claim, read
> [docs/memory/VERIFIED_STATE.md](docs/memory/VERIFIED_STATE.md) — it records what has actually been proven
> to run, and with which command.
>
> As of **2026-08-01** (`b895df1` on `main`): all 32 rows of the
> [E2E recovery plan](docs/plans/E2E_RECOVERY_PLAN.md)'s use-case matrix are green
> (`SENTINEL_E2E=1 go test -tags=e2e ./tests/e2e/` → **76 passed, 0 skipped**), covering key management,
> rate limiting, alerting, observability (structured logs, `/metrics`, a distributed trace joining
> ingestor → NATS → processor), and idempotent event writes. **CI is green on `main` — all 9 check runs** — for
> the first time in this repository's history; the previous baseline `b9e2018` was red.
>
> That last point is the whole caveat this banner exists for. The dashboard features shipped before it
> (org-wide alerts, invitation acceptance, issue relations, API-key management, member management) each
> merged with their own tests passing, and **three of the five did not execute at runtime** until the
> [UI parity remediation](docs/plans/UI_PARITY_REMEDIATION_PLAN.md) (47 findings, all closed). "Merged"
> and "reviewed" are not evidence here; a named command with an observed result is.
>
> Remaining known gaps, none of them a regression: the DLQ drainer ships gated off by default
> (`DLQ_DRAINER_ENABLED`/`DLQ_DRAINER_EXECUTE`), rate limiting is still non-atomic and fails open
> (VERIFIED_STATE.md S10), and invitations now depend entirely on working email delivery — a deliberate
> consequence of no longer returning the raw token to any client. The degradation buffer was deliberately
> deleted rather than fixed (S9).

## Repository layout

- `apps/ingestor-go` — auth, rate limiting, validation, publish to NATS. The only externally exposed service.
- `apps/processor-go` — consumes NATS, normalizes/masks/fingerprints events, writes to PostgreSQL.
- `apps/dashboard-web` — SvelteKit UI and JSON API, reads the same database.
- `packages/shared-go` — shared Postgres pool, NATS pub/sub, and Redis client code.
- `packages/proto` + `gen/` — the `ErrorEvent` wire contract (buf + protovalidate).
- `packages/db-migrations` — goose-based SQL migrations, one flat directory for every target
  (see `docs/memory/ARCHITECTURE.md`).
- `packages/sdk-go` — the public Go client SDK.
- `tests/{unit,integration,load}` — root-module tests; integration tests use testcontainers.

The root module, `packages/sdk-go`, and `packages/db-migrations` are three separate Go modules joined by a
committed `go.work` for local development (`GOWORK=off` is still used in CI-equivalent checks to exercise the
real, published-pseudo-version dependency path — see `docs/memory/ARCHITECTURE.md`).

## Prerequisites

| Tool | Version | Check |
|---|---|---|
| Go | 1.25+ (see `go.mod`) | `go version` |
| Node.js | 20+ | `node --version` |
| pnpm | 9+ | `pnpm --version` |
| Docker or Podman, with Compose v2 | any recent | `docker compose version` |
| [go-task](https://taskfile.dev) | 3.x | `task --version` |
| `jq` | any recent | `jq --version` |
| [NATS CLI](https://github.com/nats-io/natscli) | any recent | `nats --version` |

`go-task` is **not** installed by default on a fresh machine — every `task ...` command below will fail with
`command not found` until you install it. See the next section.

`jq` is a hard dependency of `scripts/wait-healthy.sh` (used by `task test-e2e` and `task infra-up` readiness
checks); without it the script exits immediately with an error. The `nats` CLI is a hard dependency of
`scripts/nats-init.sh:14` (`until nats server check connection ...`) if you ever run that script directly on
the host — without the binary the `until` loop never succeeds and the script **hangs forever** instead of
failing. In the normal `task infra-up` / `task test-e2e` flow this isn't an issue: NATS stream setup runs
inside the `nats-init` compose container, which bundles its own `nats` CLI (see
`scripts/Dockerfile.nats-init`) — the host-installed `nats` CLI is only needed if you run
`./scripts/nats-init.sh` or `task nats-init` by hand against a NATS instance you started yourself.

### Installing go-task

The commands below (verified against v3.52.0 on Linux) install a single static binary with no root
required:

```bash
# Official installer — installs into ./bin by default; -b sets the target directory.
# Put a directory that's already on your $PATH, e.g. ~/.local/bin (create it first if needed).
mkdir -p ~/.local/bin
sh -c "$(curl --location https://taskfile.dev/install.sh)" -- -d -b ~/.local/bin

# Make sure that directory is on PATH (add to your shell rc if not already):
export PATH="$HOME/.local/bin:$PATH"

task --version
```

Alternatives if you prefer a package manager:

```bash
# Arch / CachyOS
sudo pacman -S go-task

# Homebrew (macOS/Linux)
brew install go-task

# via `go install` (slow: pulls a large dependency tree; only if you already have Go and no
# package manager available)
go install github.com/go-task/task/v3/cmd/task@latest
```

If you genuinely cannot install `task`, every `task <name>` command in this repo has a raw equivalent — see
[Raw command equivalents](#raw-command-equivalents-no-go-task-required) below.

## Quickstart

```bash
git clone <this-repo>
cd sentinel

# 1. Environment
cp .env.example .env
# Edit .env only if you need to change ports/credentials; defaults work for local dev.

# 2. Start infrastructure + build and boot the app containers
#    (postgres, redis, nats, a one-shot `migrate` container that applies schema, then
#    ingestor/processor/dashboard, which wait for `migrate` to succeed before starting)
task infra-up
# or: docker compose up -d

# 3. Verify the ingestor is up
curl -s localhost:8080/health

# 4. Verify migrations landed
task db-migrate CMD=status
```

`task infra-up` brings up the whole stack including a `migrate` service that applies every pending
migration before `ingestor`/`processor`/`dashboard` are allowed to start (see the `depends_on:
service_completed_successfully` wiring in `docker-compose.yml`). `task db-migrate` (below) is for running
migrations **outside** that flow — e.g. against a `postgres` container you started by hand, or to check
status / roll back / re-baseline without restarting the whole stack.

### Common tasks

```bash
task --list                 # see every available task with its description
task build                  # build ingestor, processor, and dashboard
task test-unit               # go test ./tests/unit/...
task test-integration        # go test ./tests/integration/... (testcontainers ONLY if nothing is on :8080 — otherwise it hits your shared dev DB; use FORCE_TESTCONTAINERS=1 to isolate)
task test-e2e                # infra-up, wait healthy, test-integration, infra-down
task dev-ingestor             # go run apps/ingestor-go against your local .env
task dev-processor            # go run apps/processor-go
task dev-dashboard            # pnpm dev in apps/dashboard-web
task infra-down               # stop the compose stack
task clean                   # stop stack (removing volumes+images) and remove build artifacts
```

### Database migrations (`task db-migrate`)

Migrations live as flat, timestamp-versioned `.sql` files in `packages/db-migrations/migrations/` and are
applied by the goose-based CLI at `packages/db-migrations/cmd/migrate` (see decision "Adopt Goose for All
Database Migrations" in `docs/memory/DECISIONS.md`). The `db-migrate` task is a single, parameterized
wrapper around it — **not** five separate tasks, because `go-task` does not template task *names* (only
values inside a task body), so a Taskfile that tries `db:{{.TARGET}}-up:` as a map key registers a task
literally named `db:{{.TARGET}}-up`, which nobody can call. `TARGET` and `CMD` are ordinary task variables
instead:

```bash
task db-migrate                                        # TARGET=processor CMD=up (defaults)
task db-migrate TARGET=ingestor CMD=status
task db-migrate TARGET=dashboard CMD=down
task db-migrate TARGET=processor CMD=baseline VERSION=1716508800
task db-migrate TARGET=processor CMD=reset CONFIRM=yes  # DANGEROUS: drops and recreates the schema
```

> [!WARNING]
> `packages/db-migrations/cmd/migrate` requires **`migrate <command> [flags]`** — the command
> (`up`/`down`/`status`/`baseline`) must come *before* `-target` (and any other flag), not after. This isn't
> a standard Go `flag` package (which accepts flags before or after positional args); it's a hand-rolled loop
> in `main.go` that reads `os.Args[1]` as the command unconditionally, so `-target=processor up` fails with
> `Error: -target is required` while `up -target=processor` (or `up -target processor`) works. `task
> db-migrate` and the raw commands in this README already use the correct order — but if you invoke
> `cmd/migrate` directly (including from `docker-compose.yml`'s `migrate` service), get the order wrong and
> it will fail fast with that misleading "-target is required" error rather than a parse error.

`TARGET` selects which `DB_URL_<TARGET>` environment variable to connect with (see `.env.example`) —
`processor`, `ingestor`, and `dashboard` all point at the same single Postgres instance in local/compose dev
(one flat migration directory applies identically regardless of which DSN was used, per
`docs/memory/ARCHITECTURE.md`); this only matters if you ever point a target at a different database. If the
relevant `DB_URL_<TARGET>` isn't set in your environment or `.env`, `db-migrate` falls back to
`postgres://sentinel:changeme@localhost:5432/sentinel?sslmode=disable`, matching the default
`docker-compose.yml` credentials.

## Raw command equivalents (no go-task required)

Every `task` command above has an equivalent plain shell command. `Taskfile.yml` is the source of truth if
these drift; the ones below were verified working against a live `docker compose up -d postgres`.

```bash
# infra-up / infra-down
docker compose up -d
docker compose down

# db-migrate (defaults: TARGET=processor CMD=up)
cd packages/db-migrations
DB_URL_PROCESSOR="postgres://sentinel:changeme@localhost:5432/sentinel?sslmode=disable" \
  go run ./cmd/migrate up -target processor
# status / down / baseline work the same way:
DB_URL_PROCESSOR="postgres://sentinel:changeme@localhost:5432/sentinel?sslmode=disable" \
  go run ./cmd/migrate status -target processor
DB_URL_PROCESSOR="postgres://sentinel:changeme@localhost:5432/sentinel?sslmode=disable" \
  go run ./cmd/migrate baseline -target processor -version 1716508800

# build-ingestor / build-processor
(cd apps/ingestor-go && go build -o bin/ingestor .)
(cd apps/processor-go && go build -o bin/processor .)

# build-dashboard
(cd apps/dashboard-web && pnpm install && pnpm build)

# dev-ingestor / dev-processor
(cd apps/ingestor-go && go run .)
(cd apps/processor-go && go run .)

# dev-dashboard
(cd apps/dashboard-web && pnpm dev)

# test-unit / test-integration / test-load
go test ./tests/unit/... -v
go test ./tests/integration/... -v -count=1
go test ./tests/load/... -v -count=1

# nats-init
./scripts/nats-init.sh

# db-shell
docker compose exec postgres psql -U sentinel -d sentinel
```

## Using the Go SDK: `APIKey` vs `ProjectKey` (breaking change in v0.2.0)

`packages/sdk-go`'s `Config` has **two separate fields** that are easy to conflate — getting this wrong is
a silent 100% event-rejection rate (this was S16 in `docs/memory/VERIFIED_STATE.md`; it happened for real):

| Field | What it is | Where it travels |
|---|---|---|
| `APIKey` | The **secret** credential (`sent_live_...` for a project-scoped key, `sent_org_...` for an org-wide key) | `X-API-Key` header only — never the body |
| `ProjectKey` | The target project's **unique name** (`projects.name`) — an identifier, not a secret | Event body, `project_key` field |

```go
sentinel.Init(sentinel.Config{
    APIKey:     "sent_live_...",        // the secret — required
    ProjectKey: "my-project",           // required for an org-wide key; optional for a project-scoped
                                         // key (whose project is already fixed by the credential — a
                                         // mismatch here is rejected with 403, never silently trusted)
    Endpoint:   "http://localhost:8080/ingest",
})
```

Before v0.2.0, a single `ProjectKey` field held the secret **and** was sent as `project_key` in the body;
the server resolved that value against `projects.name`, never found it, and every event was accepted
(`202`) then silently dead-lettered. `Config.Validate()` now detects the swap (an `APIKey`-shaped value in
`ProjectKey`, or vice versa) and returns an error instead of failing silently — if you are upgrading from a
pre-v0.2.0 client, move your secret into `APIKey`.

## More documentation

- [docs/memory/VERIFIED_STATE.md](docs/memory/VERIFIED_STATE.md) — what actually runs, with proof.
- [docs/memory/ARCHITECTURE.md](docs/memory/ARCHITECTURE.md) — system boundaries and durable decisions.
- [docs/memory/DECISIONS.md](docs/memory/DECISIONS.md) — decision log (migrations, auth, tenancy, SDK, ...).
- [docs/memory/BUGS.md](docs/memory/BUGS.md) — known defects and their root causes.
- [docs/plans/E2E_RECOVERY_PLAN.md](docs/plans/E2E_RECOVERY_PLAN.md) — the active plan to make every feature
  work end-to-end and add CI.
- [packages/db-migrations/README.md](packages/db-migrations/README.md) — migration recovery procedures.
- `CLAUDE.md` — repo conventions for AI coding agents (also useful background for humans).

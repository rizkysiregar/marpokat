# Marpokat

Multi-tenant **chat as a service**: a Go backend (REST + WebSocket), SDKs for web and Android, and a protocol any
app can integrate. Narrativ is the first tenant.

Plan, decisions and progress: [`docs/PLAN.md`](docs/PLAN.md).

## Repository

| Path            | What                                                                     |
| --------------- | ------------------------------------------------------------------------ |
| `server/`       | Go backend, one binary: `marpokat api`, `worker`, `migrate`              |
| `spec/`         | The contract: `openapi.yaml` (REST) and `events.md` (WebSocket)           |
| `examples/`     | `web-chat`: a one-file client to try the chat by hand                    |
| `sdk-js/`       | `@marpokat/core` and `@marpokat/react` (from M6)                          |
| `sdk-android/`  | Kotlin SDK (from M7)                                                      |
| `docs/`         | Plan and integration docs                                                |

## Server quick start

Needs Go 1.26 and Docker.

```bash
cd server
cp .env.example .env
make deps-up        # Postgres + pgbouncer, Redis, RabbitMQ, MinIO in Docker
make migrate        # apply migrations
make run-api        # public :8080, probes and metrics :9090
make run-worker     # probes and metrics :9091
```

```bash
curl localhost:8080/v1          # {"name":"marpokat","version":"dev"}
curl localhost:9090/readyz      # {"status":"ok","checks":{"postgres":"ok","redis":"ok"}}
```

Other targets: `make help`. The ones used most:

| Target           | What                                                         |
| ---------------- | ------------------------------------------------------------ |
| `make test`      | unit tests                                                   |
| `make vet`       | `go vet` + gofmt check (what CI runs, with the tests)        |
| `make lint`      | golangci-lint (Docker)                                       |
| `make gen`       | regenerate `internal/store` from `queries/*.sql` with sqlc   |
| `make migrate-status` / `migrate-down` | inspect / roll back locally            |

## Server layout

```
server/
├── cmd/marpokat/          entry point and subcommands
├── internal/app/          wiring of api, worker, migrate
├── internal/platform/     config, db, redisx, rabbitmq, storage, httpx, health, metrics, migrate
├── internal/store/        sqlc output (generated, do not edit)
├── migrations/            goose SQL migrations, embedded in the binary
└── queries/               SQL for sqlc
```

Feature modules (`tenant`, `channel`, `message`, `realtime`, ...) are added under `internal/` milestone by
milestone, see the plan.

## Conventions

- The contract comes first: change `spec/` in the same pull request as the code that implements it.
- Never edit a migration that has run in dev; add a new one.
- Every query on tenant data filters on `tenant_id`.
- Every Redis key and Pub/Sub channel is built with `redisx.Client.Key` (prefix `marpokat:`).

## Deployment

Jenkins (`server/Jenkinsfile`) tests, builds `ghcr.io/rizkysiregar/marpokat-server`, and updates
`homelabs-gitops/apps/marpokat-dev`; Argo CD syncs dev automatically. Prod (`apps/marpokat`) is promoted by pull
request. Setup steps: `homelabs-gitops/apps/marpokat-dev/README.md`.

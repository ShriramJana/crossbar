# Crossbar

A multi-provider LLM gateway. Applications point at Crossbar instead of at a model provider directly. Crossbar authenticates the caller, enforces per-team rate limits and spending budgets, routes to a healthy provider, and fails over when one degrades.

Written in Go with the standard library `net/http` server. The circuit breaker, token bucket, and fallback logic are implemented in-repo rather than imported.

## Status

| Milestone | Scope | State |
|---|---|---|
| M0 | Skeleton, `/health`, graceful shutdown, structured logging | done |
| M1 | Provider interface, Anthropic and mock adapters | done |
| M2 | Auth, teams, config hot reload | done |
| M3 | Redis token bucket, budgets | planned |
| M4 | Circuit breaker, retry, fallback chain | planned |
| M5 | Prometheus metrics, Grafana dashboard | planned |
| M6 | Load test and measured results | planned |

## Running

```bash
make build       # builds bin/crossbar
make test        # go test -race ./...
make check       # lint + test
make run         # docker compose: crossbar, redis, prometheus, grafana
```

Once the stack is up:

```bash
curl -i http://localhost:8080/health
```

Grafana is at http://localhost:3000, Prometheus at http://localhost:9090.

## Configuration

Teams, provider endpoints, fallback tiers, pricing, and breaker tuning live in one YAML file. See [config.example.yaml](config.example.yaml) for a documented starting point. Upstream secrets are referenced by environment variable name and never stored in the file. Bearer keys are stored as SHA-256 digests:

```bash
echo -n 'your-key' | ./bin/crossbar -hash-key
```

The file is hot-reloaded on change and on `SIGHUP`. A reload is atomic: the whole file is parsed and validated first, and an invalid edit is logged and ignored while the previous configuration keeps serving.

Runtime flags (each also readable from the environment variable in parentheses):

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `-config` | `CROSSBAR_CONFIG` | `config.yaml` | path to the config file |
| `-addr` | `CROSSBAR_ADDR` | `:8080` | listen address |
| `-log-format` | `CROSSBAR_LOG_FORMAT` | `text` | `json` or `text` |
| `-log-level` | `CROSSBAR_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `-drain-timeout` | | `10s` | grace period for in-flight requests on shutdown |

## API

Data plane, authenticated with a team key (`Authorization: Bearer <key>`):

| Route | Purpose |
|---|---|
| `POST /v1/messages` | Anthropic-compatible chat request (routing lands in M4) |

Control plane, authenticated with the admin key:

| Route | Purpose |
|---|---|
| `GET /health` | liveness, no auth |
| `GET /admin/teams/{id}/usage` | a team's configured limits and current spend |
| `POST /admin/config/reload` | force a config reload; `422` with the reason if the file is invalid |

Unknown or missing keys get a `401` with no further detail.

## Results

Measured numbers land here once the load test exists (M6). Every figure will state the parameters it was measured under.

## Design notes

See [docs/decisions.md](docs/decisions.md) for the trade-offs behind the larger choices.

# Crossbar

A multi-provider LLM gateway. Applications point at Crossbar instead of at a model provider directly. Crossbar authenticates the caller, enforces per-team rate limits and spending budgets, routes to a healthy provider, and fails over when one degrades.

Written in Go with the standard library `net/http` server. The circuit breaker, token bucket, and fallback logic are implemented in-repo rather than imported.

## Status

| Milestone | Scope | State |
|---|---|---|
| M0 | Skeleton, `/health`, graceful shutdown, structured logging | done |
| M1 | Provider interface, Anthropic and mock adapters | done |
| M2 | Auth, teams, config hot reload | done |
| M3 | Redis token bucket, budgets | done |
| M4 | Circuit breaker, retry, fallback chain | done |
| M5 | Prometheus metrics, Grafana dashboard | planned |
| M6 | Load test and measured results | planned |

## Running

```bash
make build       # builds bin/crossbar
make redis       # starts a local Redis for the limiter and budget tests
make test        # go test -race ./...
make check       # lint + test
make run         # docker compose: crossbar, redis, prometheus, grafana
```

Tests that exercise the Redis-backed limiter and budgets skip when no Redis is reachable on `localhost:6379` (override with `REDIS_ADDR`). Everything else runs hermetically against the mock provider; no test needs a provider API key.

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
| `-redis` | `REDIS_ADDR` | `localhost:6379` | Redis for rate limits and budgets |
| `-request-timeout` | | `60s` | end-to-end bound on one request, shared by every retry and fallback |
| `-log-format` | `CROSSBAR_LOG_FORMAT` | `text` | `json` or `text` |
| `-log-level` | `CROSSBAR_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `-drain-timeout` | | `10s` | grace period for in-flight requests on shutdown |

## API

Data plane, authenticated with a team key (`Authorization: Bearer <key>`):

| Route | Purpose |
|---|---|
| `POST /v1/messages` | Anthropic-compatible chat request. Responds with `X-Crossbar-Provider`, `X-Crossbar-Model`, `X-Crossbar-Fallback`, and `X-Crossbar-Cost-USD` headers |

Rejections on the data plane:

| Status | Body `error` | When |
|---|---|---|
| `401` | `unauthorized` | unknown or missing key |
| `403` | `model_not_allowed` | model outside the team's `allowed_models` |
| `429` | `rate_limited` | requests or tokens per minute exhausted; `Retry-After` and `dimension` say which |
| `402` | `budget_exhausted` | daily or monthly spend reached; `period` and `resets_at` say which and when |
| `400` | `unknown_model` | no tier lists the model |
| `503` / `504` | `upstream_unavailable` / `upstream_timeout` | every target in the chain failed or was skipped, or the request deadline passed; `attempted` lists each target and why |

## Resilience

A request names a model; the model resolves to a tier, and the tier is a fallback chain of `(provider, model)` targets. On a retryable failure (429, 5xx, timeout, connection error) the same target is retried with full-jittered exponential backoff, then the chain moves on. A non-retryable failure (4xx) goes straight back to the caller. The whole walk shares one deadline.

Each target has its own circuit breaker, written in [internal/breaker](internal/breaker/breaker.go). Closed admits everything and counts outcomes over a rolling window; once the failure ratio crosses the threshold with enough traffic behind it, the breaker opens and rejects instantly. After a cooldown it admits one probe: success closes it, failure reopens it with the cooldown doubled up to a cap. Open breakers are skipped in the chain without an upstream call.

A background prober health-checks every provider on an interval. Down providers are excluded from routing; degraded ones are tried after healthy ones.

| Route | Purpose |
|---|---|
| `GET /health/providers` | per-provider status, error rate, p99 probe latency; every breaker's state and window counts. No auth |
| `POST /admin/breakers/{provider}/{model}/reset` | force a breaker closed |

Control plane, authenticated with the admin key:

| Route | Purpose |
|---|---|
| `GET /health` | liveness, no auth |
| `GET /admin/teams/{id}/usage` | a team's configured limits and current spend |
| `POST /admin/config/reload` | force a config reload; `422` with the reason if the file is invalid |

Unknown or missing keys get a `401` with no further detail.

## Results

Every figure states how it was measured. The load-test numbers (gateway overhead, throughput, failover latency) land with M6.

| Measure | Result | Method |
|---|---|---|
| Rate limit accuracy under concurrency | 100 of 1000 admitted (exact, 8 of 8 runs) | `TestConcurrentAccuracy`: 1000 goroutines released simultaneously against a fresh 100 rpm bucket on a local Redis 7, `-race` on. Spec tolerance was ±2. |

## Design notes

See [docs/decisions.md](docs/decisions.md) for the trade-offs behind the larger choices.

# Relay

A multi-provider LLM gateway. Applications point at Relay instead of at a model provider directly. Relay authenticates the caller, enforces per-team rate limits and spending budgets, routes to a healthy provider, and fails over when one degrades.

Written in Go with the standard library `net/http` server. The circuit breaker, token bucket, and fallback logic are implemented in-repo rather than imported.

## Status

| Milestone | Scope | State |
|---|---|---|
| M0 | Skeleton, `/health`, graceful shutdown, structured logging | done |
| M1 | Provider interface, Anthropic and mock adapters | planned |
| M2 | Auth, teams, config hot reload | planned |
| M3 | Redis token bucket, budgets | planned |
| M4 | Circuit breaker, retry, fallback chain | planned |
| M5 | Prometheus metrics, Grafana dashboard | planned |
| M6 | Load test and measured results | planned |

## Running

```bash
make build       # builds bin/relay
make test        # go test -race ./...
make check       # lint + test
make run         # docker compose: relay, redis, prometheus, grafana
```

Once the stack is up:

```bash
curl -i http://localhost:8080/health
```

Grafana is at http://localhost:3000, Prometheus at http://localhost:9090.

## Configuration

Runtime flags (each also readable from the environment variable in parentheses):

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `-addr` | `RELAY_ADDR` | `:8080` | listen address |
| `-log-format` | `RELAY_LOG_FORMAT` | `text` | `json` or `text` |
| `-log-level` | `RELAY_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `-drain-timeout` | | `10s` | grace period for in-flight requests on shutdown |

## Results

Measured numbers land here once the load test exists (M6). Every figure will state the parameters it was measured under.

## Design notes

See [docs/decisions.md](docs/decisions.md) for the trade-offs behind the larger choices.

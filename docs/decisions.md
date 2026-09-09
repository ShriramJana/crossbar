# Design decisions

Short entries, newest last. Each one records a choice that had a real trade-off and why it went the way it did.

## Standard library HTTP server, no framework

`net/http` in Go 1.22 has method-aware pattern routing (`GET /health`), which covers every route this gateway needs. A framework would add a dependency, a second middleware model, and nothing the project uses. Middleware is plain `func(http.Handler) http.Handler`.

## Graceful shutdown drains in-flight requests with a bounded timeout

On SIGINT/SIGTERM the server stops accepting connections and waits for in-flight requests up to a configurable drain timeout (default 10s). Waiting forever risks a hung deploy; not waiting drops LLM calls that may have already been billed upstream. A bounded wait is the compromise, and the timeout is logged if it fires.

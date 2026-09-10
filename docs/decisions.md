# Design decisions

Short entries, newest last. Each one records a choice that had a real trade-off and why it went the way it did.

## Standard library HTTP server, no framework

`net/http` in Go 1.22 has method-aware pattern routing (`GET /health`), which covers every route this gateway needs. A framework would add a dependency, a second middleware model, and nothing the project uses. Middleware is plain `func(http.Handler) http.Handler`.

## Graceful shutdown drains in-flight requests with a bounded timeout

On SIGINT/SIGTERM the server stops accepting connections and waits for in-flight requests up to a configurable drain timeout (default 10s). Waiting forever risks a hung deploy; not waiting drops LLM calls that may have already been billed upstream. A bounded wait is the compromise, and the timeout is logged if it fires.

## Provider adapters speak raw HTTP, not vendor SDKs

Each adapter marshals the normalized `Request` into the provider's wire format with `net/http` and `encoding/json`. Vendor SDKs would pull in large dependency trees, hide the status code and body the gateway needs for error classification, and add their own retry loops that would fight the gateway's. The wire formats are small and stable; owning them is cheaper than owning the SDKs.

## Error classification is by HTTP status, decided in one place

`ClassifyStatus` is the single mapping from upstream status to `ErrorClass`: 5xx and non-standard codes above 500 (Anthropic's 529) are retryable, 408 and 429 are retryable, every other 4xx is the caller's fault. Context expiry during a call is treated as retryable (it looks like an upstream timeout); any other transport failure is `ErrTransport`. Keeping this in one function means every adapter and the breaker agree on what counts as the provider's fault.

## Anthropic health checks hit `GET /v1/models`, not the Messages endpoint

A background prober runs every 30 seconds per provider. Sending even a one-token message would be a billable call each time; listing models is free, still exercises auth and connectivity, and returns the same status codes on outage. The trade-off is that it does not prove inference works, which the live traffic and breaker already cover.

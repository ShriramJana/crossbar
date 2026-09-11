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

## Config watcher watches the directory and reloads on any event for the file name

Editors and deploy tools save by writing a temp file and renaming it over the original. A watch on the file itself follows the old inode and goes silent after the first save. Watching the parent directory and filtering by base name survives that. The backends also disagree on how a rename-replace surfaces (inotify reports Create, kqueue reports Remove then Create), so any event for the name schedules a debounced reload rather than matching specific operations. A reload that finds the file unchanged is a no-op; one that finds it momentarily absent logs and keeps the old config.

## Unknown config fields are a hard error

`requests_per_min` instead of `requests_per_minute` would otherwise parse cleanly and leave the team with a zero limit, which validation catches, or silently drop an `allowed_models` restriction, which nothing would catch. Strict decoding turns a typo into a rejected reload with the field name in the error. The cost is that adding a field requires a code change before the file can mention it, which is the intended coupling.

## Empty `allowed_models` means every model

Deny-by-default would be the safer reading, but it makes the common case (a team that may use anything in its tiers) verbose and easy to get wrong when a new model is added. The rate limits and budgets are the real blast-radius controls; the model list is a convenience restriction. This is documented next to the field in the example config.

## Rate limiting is one Lua script, and limiter tests run against a real Redis

The check-and-decrement for both dimensions (requests and tokens) runs in a single `EVALSHA` so that 1000 concurrent callers cannot interleave a read and a write. A read-then-write from Go would admit more than the limit under load, which is exactly the case the gateway exists for. Because the atomicity lives in Lua, an in-process Redis stand-in would test a reimplementation rather than the shipped code, so the limiter and budget tests connect to a real Redis and skip when none is reachable. `go test ./...` still passes on a bare machine; `make redis` starts one for the full run.

The bucket stores its clock as Redis server time in microseconds. Lua's `tostring` renders a number that large as `1.7576e+15`, which lost precision and broke parsing; the script formats the fields explicitly. Worth knowing before writing the next script.

## Token reservations are optimistic and reconciled after the call

A request reserves `max_tokens` up front and refunds the unused part once the provider reports actual usage. Reserving nothing would let a burst of large requests through; reserving only the input would ignore the output, which is the expensive half. Overspend beyond the reservation is charged back, and the bucket is clamped to plus or minus one minute's capacity so a single bad reservation can neither lock a team out for long nor bank credit.

## Limiter and budget failures fail open

If Redis is unreachable, the request proceeds and the failure is logged. Failing closed would turn every Redis hiccup into a full outage of the gateway, which exists to prevent outages. The cost is that limits and budgets are unenforced for the duration of a Redis outage. Envoy's rate limit filter defaults the same way for the same reason.

## Budgets are exhausted at 100%, warned at 80%, and zero means unlimited

The 80% warning is a `SETNX` on a marker key with the period's TTL, so it fires once per period across every gateway instance rather than once per process. Reaching the limit exactly counts as exhausted. A zero budget disables the check rather than rejecting everything, so a team can be given rate limits without a spend cap.

## A requested model leads its chain; a tier name selects the whole chain

A request naming a model resolves to the tier that lists it, with that target first and the tier's other targets following in configured order. A request naming a tier gets the chain as configured. When two tiers list the same model, the tier listing it earliest wins, with ties broken by tier name so the answer is deterministic and does not depend on map iteration order.

## Every tier target must have a pricing entry

Validation rejects a config that routes to a (provider, model) with no price. The alternative, pricing unknown targets at zero, would silently leave spend unattributed, which is one of the four problems in the product brief.

# Local Production Reliability

## Build Identity

The root `make build` builds frontend assets before the embedded backend.
Supply an explicit release `VERSION`, `COMMIT` label and UTC `BUILD_DATE`.
The binary's version, SHA-256, loaded mapping and consumer receipt must agree.
The frontend package manager is pinned; security overrides live in
`frontend/pnpm-workspace.yaml` and installed versions are locked.
SheetJS uses a reviewed versioned upstream tarball, not the old npm release.
Dependency installation does not authorize arbitrary lifecycle scripts.

## Capacity And Accounting

- Shared classification caps same-account capacity retries at one.
- One request stops after three capacity failures across account changes.
- Quota/authentication classification and configured 429 cooldowns are retained.
- Committed output is never silently replayed, and terminal errors retain
  their protocol meaning.
- Gateway `total_cost`/`actual_cost` are accounting values. In SIMPLE mode
  they do not prove upstream cash charges; that requires provider billing.

## Shutdown

`SERVER_SHUTDOWN_TIMEOUT` accepts a positive Go duration up to one hour.
The default is five seconds; inspect the target launcher or environment for
its actual override. HTTP and upgraded WebSocket shutdown are separate checks.

The registry lives in the real request context, covers coder WebSocket and
Gorilla management monitoring, and retains pending upgrades until their handler
exits. Each Responses forwarder invocation acquires an immutable attempt
generation before its first request. A turn is released only after writing its
terminal frame to the client, not when accounting observes completion.
Stale callbacks cannot release newer attempts or turns.

Management connections close with service-restart semantics. Continuous
realtime sessions stay active until completion or deadline. Forced closure
and timeout are failures, never successful drain evidence.

## Deployment

Do not force-restart active inference requests. Verify the old process,
executable generation and connections, preserve the prior binary and startup
binding, then verify the new mapping and hash. Validate real API completion,
model identity, frontend assets, authentication and current metrics.

Compilation, tests, a listening port, a 200 health result, a short quiet
window or a versioned filename are not sufficient production acceptance alone.

## Regression Scope

Tests cover three native Responses WS modes, first-turn registration, blocked
real client terminal writes, management WS closure, token-list/multi-value
Upgrade headers, late registration, generation-safe callbacks, cancellation,
deadlines, capacity limits, Excel exports and accounting precision.
These bounded tests do not prove every workload or an actual 55-minute drain.

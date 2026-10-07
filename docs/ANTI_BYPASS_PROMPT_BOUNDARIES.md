# Gateway Anti-Bypass Boundaries

The gateway checks authenticated request identity, request budgets and caller-supplied
instruction text when anti-bypass protection is enabled. These checks are bounded
rules; they do not guarantee semantic safety of every possible prompt.

## Prompt inspection

- Inspect each caller instruction message independently, including supported system,
  developer and instruction fields. Join text blocks within a message before inspection.
- Match override/disclosure/activation actions with their instruction or security objects.
  Negation applies only to the adjacent action in the same clause.
- Normalize Unicode, full-width and separated-letter text with bounded repeated decoding.
- Explicit quoted translation, analysis and test fixtures can treat quotes as data;
  commands outside quotes and requests to execute quoted instructions remain subject to checks.
- Tool results, assistant history and non-text media are data. No AGENTS filename or
  complete document is whitelisted. Server-added instructions are outside inbound inspection.
- Accepted HTTP bodies and WebSocket frames retain their original bytes.

## Identity and limits

- HTTP `X-Client-Request-ID` is a tracing header, not an operation identity or attempt budget.
  Explicit `Idempotency-Key` conflicts and WebSocket `event_id` conflicts remain checked.
- Content-Type, Accept and Accept-Language do not participate in the client fingerprint.
- Replay identity uses the authenticated API key within the user scope. User RPM,
  concurrency, distinct key/IP/client and cross-key replay policies remain separate.
- HTTP wire limits use `gateway.max_body_size` (default 256 MiB), bounded by the server limit.
- Responses frames use `gateway.openai_ws.client_read_limit_bytes` (default 64 MiB),
  bounded by the wire limit. Text inspection uses `gateway.text_max_body_size`
  (default 32 MiB), bounded by the wire/frame limit. These are not model context guarantees.
- Exceeded text/depth budgets and unsupported nested structures fail explicitly.

## Responses WebSocket lifecycle

Accepted create frames, including supported implicit forms, acquire an inference lease.
Control frames do not consume inference quota. Leases belong to exact accepted turn numbers;
old completions cannot release newer work, including across protection switch changes.

Completion and cleanup release the matching leases in native, HTTP-bridge and passthrough
paths. Upstream retries within a turn do not count as new client admissions. Connection cleanup
cancels renewal and releases unfinished turns. Independent shutdown draining waits for the
terminal frame to be written to the client. Realtime retains its connection policy.

## Errors

| Condition | Error |
| --- | --- |
| Wire overflow | HTTP 413 / WS message-too-big, `ANTI_BYPASS_BODY_TOO_LARGE` |
| Text/depth budget | `ANTI_BYPASS_INSPECTION_LIMIT` |
| Prompt rule match | `ANTI_BYPASS_PROMPT_BLOCKED` |
| WS request budget | `ANTI_BYPASS_BLOCKED`, with machine-readable reason |
| Settings/Redis unavailable | `ANTI_BYPASS_UNAVAILABLE`; no fail-open fallback |

## Verification and operation

Regression entry points include `TestPromptBenignActionBoundaries`,
`TestPromptNegationDoesNotExemptOtherActions`, `TestAntiBypassPromptBoundariesPreserveHTTPBody`
and `TestAntiBypassInspectsWebSocketFirstAndFollowupFrames`.

Preserve configured switches and Redis budget/lease state. Mixed binary generations can
apply different identity rules until bounded replay/fingerprint windows expire; clearing
Redis is not a substitute for a verified rollout. Deployment checks are described in
[Production Reliability](PRODUCTION_RELIABILITY.md).

# Anti-Bypass Prompt Boundary Repair

The gateway prompt detector previously combined unrelated words across user
messages and treated negated instructions as attacks. For example, the title
`AGENTS.md instructions` and an unrelated Chinese statement about not
overwriting boundaries could jointly trigger a 403.

## Changed Behavior

- Inspect each user message independently. Text blocks within one message are
  still joined so splitting an attack into content blocks does not exempt it.
- Match an override/disclosure/activation action with its instruction or
  security object, rather than combining words anywhere in the whole request.
- Negation and defensive wording apply only to the adjacent action, within
  the same clause. A benign sentence cannot exempt a later attack.
- Preserve Unicode, full-width and separated-letter normalization, bounded
  repeated Unicode decoding, and the explicit quoted-security-analysis rule.
  That analysis rule is scoped to its own message, not the whole conversation.
- Leave the original request body and accepted WebSocket frames byte-for-byte
  unchanged. No AGENTS filename or complete document is whitelisted.

The enabled/disabled setting, billing, identity attribution, replay protection,
Redis rate/concurrency limits, and failure handling are unchanged. System,
developer and tool-output roles retain their existing treatment. This rule
does not prove semantic safety of every possible prompt.

## Verification

The permanent regression suites are `TestPromptBenignActionBoundaries`,
`TestPromptNegationDoesNotExemptOtherActions`,
`TestAntiBypassPromptBoundariesPreserveHTTPBody`, and
`TestAntiBypassInspectsWebSocketFirstAndFollowupFrames`.

The user's actual attachment was tested through a separate local Go overlay,
including raw text, JSON/Unicode, history, quoted text, tool output, HTTP and
real loopback WebSocket frames. That private attachment is not checked into
this repository. An attack appended to the attachment must still be rejected.

Source tests and a compiled candidate do not mean the running gateway has
adopted the change. Preserve the current switch state. The single gateway
publisher must deploy a versioned candidate while preserving the latest
connection/retry changes, verify loaded binary identity and consumer behavior,
and retain the previous binary for rollback. Do not publish an older R7 build
over newer connection fixes.

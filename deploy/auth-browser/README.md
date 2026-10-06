# Sub2API macOS authorization browser launcher

This optional helper is used by `SUB2API_AUTH_BROWSER_LAUNCHER` to open an
isolated Google Chrome profile for an OpenAI OAuth session.

The launcher is deliberately fail-closed:

- the caller must provide an explicit no-auth proxy ingress;
- the proxy is tested before a profile is created;
- no direct or default proxy fallback exists;
- only `https://auth.openai.com/oauth/authorize` URLs are accepted;
- the isolated profile tag must match the OAuth `state` carried by that URL;
- a running profile cannot be silently reused with another proxy.

Install the directory on the macOS host, keep `launch.sh` executable, and set:

```bash
export SUB2API_AUTH_BROWSER_LAUNCHER="/opt/sub2api/auth-browser/launch.sh"
```

Optional paths can be overridden with `SUB2API_AUTH_BROWSER_CHROME`,
`SUB2API_AUTH_BROWSER_CURL`, `SUB2API_AUTH_BROWSER_PYTHON`,
`SUB2API_AUTH_BROWSER_PROFILE_ROOT`, and
`SUB2API_AUTH_BROWSER_LOG_FILE`. Disposable `auth-<state fingerprint>`
profiles older than 72 hours are removed before launch when they have no live
or ambiguous Chrome lock. Override the retention with
`SUB2API_AUTH_BROWSER_PROFILE_MAX_AGE_HOURS` (1-8760).

For fixed local proxy buckets, bind each ingress port to its expected public
exit and require the check before Chrome starts:

```bash
export SUB2API_AUTH_BROWSER_REQUIRE_STATIC_EXIT_CHECKS=true
export SUB2API_AUTH_BROWSER_EXPECTED_EXIT_17931="192.0.2.10"
```

When the requirement flag is enabled, ports `17931` through `17934` fail
closed if their expected exit is missing or differs from the observed IP.

## Native Reauthorization Candidate

For account-bound reauthorization, the host passes a fourth positional
argument containing the original login IP. This pin is mandatory for that
flow, including non-static ingress ports. It is checked in addition to the
ingress expectation; neither can override the other. An empty or changed
original IP aborts before Chrome is started.

Reauthorization also requires one-use evidence of a successful host launch
for that exact account, revision, OAuth session and route. Missing, failed,
expired or other-session launches cannot exchange a code or issue a replacement
proof. The account and route are checked again when the launcher completes.
Old replacement proofs without this evidence require a new authorization.
After an exchange attempt the UI discards the session, including on a network
error; it never silently replays the code or starts another login.

The candidate host uses the system-managed account extra field
`openai_oauth_login_exit_ip`, not the latest `proxies.exit_ip` observation.
Accounts without verified historical evidence fail closed. Do not populate
this field from today's live probe or from an arbitrary account import.
Historical evidence recovery is not implemented in this candidate. New
identities can establish the login IP only through the reviewed fixed-route
path below; this cannot backfill missing history for a deleted identity.

For a new identity on a reviewed fixed route, a successful host browser launch
records one-use launch evidence. Token exchange consumes that evidence and
issues a five-minute, one-use creation proof bound to the returned identity,
credentials and proxy route. The create transaction rechecks the route and
history before persisting the login IP. Copying the authorization URL, sending
an IP field, or importing credentials cannot establish this history. Failed
creation returns the UI to the preserved form; check for an already-created
account before starting authorization again rather than replaying the code.

Launch evidence attests to the host launch, not to every subsequent human
action. Do not copy the link into another browser or network after launching.
A fully automated adapter must drive the same reviewed browser/route through
the callback; a detached credential upload cannot establish that continuity.

This is a native workflow adaptation, not execution or recompilation of
codex-helper 0.2.32. The optional automatic mode is described below. IP probes
detect drift at checked boundaries, not every browser packet: real adoption
also requires a fixed-egress route with no identity-changing fallback.
The rescue transport plugin is not replaced or given a second outbound owner.

### Automatic Reauthorization Candidate

Both OpenAI OAuth reauthorization dialogs include a form for the account
email, password and optional authenticator-app TOTP secret. Submission creates
an account-bound session, drives an isolated Chrome window through the same
reviewed fixed exit, exchanges the callback once, and applies the host's
one-use replacement proof to the original account. Account identity, route,
original IP and authorization revision must still match. A successful browser
launch alone is not a successful account update.

Automatic mode requires Node.js 22 or newer on the macOS Sub host, in addition
to the existing Chrome, curl and Python requirements. Set
`SUB2API_AUTH_BROWSER_NODE` to an absolute Node executable path when the service
environment has no Node on PATH. The browser opens on the Sub host, not on a
remote user's computer. Linux and Windows archives contain the source and
adapter but do not provide a native browser launcher.

The form is enabled only for OpenAI OAuth accounts. Input is submitted only
from HTTPS or numeric/local loopback UI origins, kept out of browser storage,
and cleared on submission, cancellation, account changes and unmount. The
server marks the response `no-store`, limits the body to 16 KiB and passes
the transient input through subprocess stdin, not command arguments,
environment or diagnostic output. Interactive requests are never replayed
by the admin-session refresh interceptor. This minimizes retention; it is not
a claim of guaranteed erasure from garbage-collected memory.

The host supplies an explicit child-environment allowlist. Service settings,
inherited proxy overrides, interpreter injection settings and an inherited
automatic-mode flag are excluded. Executable paths, desktop/runtime settings
and canonical per-port exit-IP pins are retained. Profile directories must
belong to the current user; existing directories are tightened to mode 0700
through no-follow directory handles. Symlinks and non-directories are rejected.

The authorization URL still appears in launcher/helper process arguments and,
in manual mode, Chrome arguments. It contains OAuth state and the public PKCE
challenge, not the PKCE verifier, login input or issued tokens. The state hash
in profile names reduces persistent disclosure; it does not conceal local
process arguments. Run the host under a trusted OS account and do not export
process command lines into diagnostics. Account-bound session validation, the
private server-side verifier, one-use browser proof and conditional replacement
remain required; observing a launch is not permission to replace an account.

Only forms on `auth.openai.com` are eligible for automatic entry. Authenticator
codes are generated locally. Third-party sign-in, email/SMS challenges,
CAPTCHAs and security challenges remain manual; nothing bypasses them.
Uncertain submissions are not repeated. The operation has a five-minute
server bound and closes its disposable browser profile on termination.
Same-account launches in different sessions are serialized while the launcher
is in flight; unrelated accounts and initial-login sessions remain independent.
This is a per-host launcher guard, not a distributed OAuth lease.

No automatic flow can recreate a missing historical login IP, repair an
upstream revocation or guarantee that an account will remain authorized.
Missing history, changed identity or an unreviewed route fails closed without
overwriting the account. Real upstream OAuth is not covered by local fixtures.

```bash
node --test tests/automate.test.mjs
```

### Fixed Route Admission

Account-bound reauthorization also requires startup configuration under
`gateway.auth_browser_fixed_egress_routes`. Each entry contains `proxy_id`,
`proxy_route_sha256`, `browser_ingress`, and `exit_ip`. The route hash is the
lowercase SHA-256 hex digest of the exact proxy URL produced by the host
(including authentication and URL escaping); never log that URL or its
credentials. `browser_ingress` must be the exact no-auth URL selected by the
launcher, and `exit_ip` must equal the account's historical login IP.

The operator must first verify the actual proxy configuration and both routes:
they must use the same fixed public exit for the entire OAuth flow, with no
direct fallback, rotating exit, or failover to a different public IP. Failover
of the transport under a fixed ISP endpoint is allowed only if the public
exit remains unchanged. Two matching live probes are not this verification.
After a routing change, re-review the route before updating its startup pin.
An unchanged proxy URL cannot detect changes hidden inside a proxy service.

Missing, malformed, or duplicate route entries disable reauthorization rather
than trusting current IP observations. Normal account editing and token refresh
do not use this route list. Initial authorization without a reviewed route
retains the existing flow but does not establish a trusted login IP; with a
reviewed route it requires the host-launched browser and creation proof.
The list cannot establish missing historical login evidence. Activation
requires a verified route and historical evidence for each target account;
this candidate does not ship fabricated production pins.

Run the focused regression tests with:

```bash
/usr/bin/python3 tests/test_launcher.py
```

### Bound Recovery Adapter

`reauthorize.mjs` adapts the supplied onboarding worker's recovery operation to
this host, rather than installing its independent protocol-login, registration,
SMS or credential-upload services. Its classification retains the supplied MIT
license in `ONBOARDING-LICENSE`; this is not an official-plugin provenance claim.

The command runs on the Sub host with Node.js supporting built-in `fetch` and
`AbortSignal.any`. Provide a protected stdin JSON object with `admin_session`
and invoke `node reauthorize.mjs --base-url http://127.0.0.1:18420 --account-id ID`.
Do not put the admin session in arguments, shell history, logs or a package.
Only numeric loopback host addresses are accepted, and redirects are refused.

For one explicitly selected 401 OAuth account it captures the initial revision,
asks Sub to create the bound OAuth session, reserves that session's loopback
callback, and invokes Sub's existing fixed-egress browser launcher. After the
browser callback, Sub exchanges the code and returns the exact credentials
bound to its one-use proof. The adapter submits them once with the original
revision. It never rereads a newer revision to make an obsolete login succeed.
Success requires a strictly newer commit revision, compared without losing
sub-millisecond precision or treating timezone spelling changes as an update.
The proof and host transaction retain account, route, original-IP, identity,
cache invalidation, rescue and scheduling guards.

The adapter does not read passwords or bypass consent/MFA. Browser sign-in may
still require the account owner; the callback, token exchange and credential
replacement are automatic. It is a local command, not yet an installed UI
plugin or background account scanner. The bundled launcher remains macOS-only;
shipping the adapter in a Windows/Linux archive is not native browser support.

The operation expires after five minutes. A busy callback port stops it before
browser launch. Account/proxy/revision changes, missing history or fixed-egress
pins, invalid proofs and cancellation fail closed. An ambiguous exchange or
commit is never retried; check current account state before starting a new
operation. Reauthorization of an unschedulable rescue account does not send
scheduling changes or erase its rescue marker. This command does not establish
missing historical login IPs.

Run adapter and real-loopback callback tests with:

```bash
node --test tests/reauthorization.test.mjs
```

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

Run the focused regression tests with:

```bash
/usr/bin/python3 tests/test_launcher.py
```

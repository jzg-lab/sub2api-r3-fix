#!/usr/bin/env python3
"""Bound the existing transactional account-pool policy before host startup."""

import os
import signal
import subprocess
import sys
import time

WALL_TIMEOUT = 120
TERMINATE_GRACE = 5


def run_policy(command, environment, timeout=WALL_TIMEOUT, grace=TERMINATE_GRACE):
    env = dict(environment)
    env["PGCONNECT_TIMEOUT"] = "10"
    env["PGOPTIONS"] = (
        env.get("PGOPTIONS", "")
        + " -c lock_timeout=10000 -c statement_timeout=60000"
        + " -c idle_in_transaction_session_timeout=60000"
    ).strip()
    env["PGAPPNAME"] = "sub2api-startup-policy"
    child = None
    interrupted = None
    launching = True
    handlers = {}
    started = time.monotonic()

    def stop(signum, _frame):
        nonlocal interrupted
        interrupted = signum
        if not launching:
            raise InterruptedError

    def terminate():
        if child is None or child.poll() is not None:
            return
        # psql SIGINT sends a PostgreSQL CancelRequest before closing libpq.
        # SIGTERM alone can leave a long server query running with its locks.
        for signum in (signal.SIGINT, signal.SIGTERM):
            try:
                os.killpg(child.pid, signum)
            except ProcessLookupError:
                pass
            try:
                child.wait(timeout=grace)
                return
            except subprocess.TimeoutExpired:
                pass
        try:
            os.killpg(child.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        child.wait()

    try:
        for signum in (signal.SIGTERM, signal.SIGINT):
            handlers[signum] = signal.signal(signum, stop)
        # Discard database output: errors may include account data or SQL values.
        child = subprocess.Popen(
            command, env=env, stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
            start_new_session=True,
        )
        launching = False
        if interrupted is not None:
            raise InterruptedError
        status = child.wait(timeout=timeout)
        outcome = "completed" if status == 0 else "database_error"
        code = 0 if status == 0 else 75
    except subprocess.TimeoutExpired:
        outcome, code = "wall_timeout", 75
    except InterruptedError:
        outcome, code = "interrupted", 128 + interrupted
    except OSError:
        outcome, code = "launch_error", 78
    finally:
        # Ignore a second stop while reaping the one child owned by this runner.
        for signum in handlers:
            signal.signal(signum, signal.SIG_IGN)
        terminate()
        for signum, handler in handlers.items():
            signal.signal(signum, handler)
    print(
        f"Sub2API startup policy: {outcome}; elapsed={time.monotonic() - started:.3f}s",
        file=sys.stderr,
    )
    return code


def main():
    if len(sys.argv) != 3:
        print("Sub2API startup policy: invalid arguments", file=sys.stderr)
        return 78
    psql, policy = sys.argv[1:]
    env = os.environ.copy()
    required = ("DATABASE_HOST", "DATABASE_PORT", "DATABASE_USER",
                "DATABASE_DBNAME", "DATABASE_PASSWORD")
    if (any(not env.get(key) for key in required)
            or env["DATABASE_HOST"] not in ("127.0.0.1", "localhost", "::1")
            or not os.path.isfile(policy) or not os.access(policy, os.R_OK)):
        print("Sub2API startup policy: invalid local configuration", file=sys.stderr)
        return 78
    env["PGPASSWORD"] = env["DATABASE_PASSWORD"]
    return run_policy([
        psql, "--no-psqlrc", "--no-password",
        "--host", env["DATABASE_HOST"], "--port", env["DATABASE_PORT"],
        "--username", env["DATABASE_USER"], "--dbname", env["DATABASE_DBNAME"],
        "--set", "ON_ERROR_STOP=1", "--file", policy,
    ], env)


if __name__ == "__main__":
    sys.exit(main())

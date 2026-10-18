import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
import unittest

SPEC = importlib.util.spec_from_file_location(
    "startup_policy", Path(__file__).with_name("startup_policy.py")
)
policy = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(policy)


class StartupPolicyTests(unittest.TestCase):
    def run_command(self, script, **kwargs):
        output = io.StringIO()
        with contextlib.redirect_stderr(output):
            result = policy.run_policy(
                [sys.executable, "-c", script], os.environ, **kwargs
            )
        return result, output.getvalue()

    def test_success_and_output_redaction(self):
        code, output = self.run_command(
            'import sys; print("private-output"); print("private-error", file=sys.stderr)'
        )
        self.assertEqual(code, 0)
        self.assertIn("completed", output)
        self.assertNotIn("private", output)

    def test_database_failure_fails_closed(self):
        code, output = self.run_command("raise SystemExit(3)")
        self.assertEqual(code, 75)
        self.assertIn("database_error", output)

    def test_missing_binary_has_configuration_error(self):
        with contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(policy.run_policy(["/does-not-exist-psql"], {}), 78)

    def test_environment_is_preserved_with_enforced_timeouts(self):
        with tempfile.TemporaryDirectory() as directory:
            result_file = Path(directory) / "env.json"
            env = {"PATH": os.environ["PATH"], "PGOPTIONS": "-c search_path=public",
                   "PGGSSENCMODE": "disable", "KEEP_EXISTING": "yes"}
            with contextlib.redirect_stderr(io.StringIO()):
                code = policy.run_policy([
                    sys.executable, "-c",
                    "import json,os,sys; json.dump(dict(os.environ),open(sys.argv[1],'w'))",
                    str(result_file),
                ], env)
            observed = json.loads(result_file.read_text())
            self.assertEqual(code, 0)
            self.assertEqual(observed["PGGSSENCMODE"], "disable")
            self.assertEqual(observed["KEEP_EXISTING"], "yes")
            self.assertEqual(observed["PGCONNECT_TIMEOUT"], "10")
            self.assertIn("-c search_path=public", observed["PGOPTIONS"])
            self.assertTrue(observed["PGOPTIONS"].endswith(
                "-c idle_in_transaction_session_timeout=60000"))
            self.assertNotIn("PGCONNECT_TIMEOUT", env)

    def test_wall_timeout_reaps_child_that_ignores_term(self):
        started = time.monotonic()
        code, output = self.run_command(
            "import signal,time; signal.signal(signal.SIGINT,signal.SIG_IGN); "
            "signal.signal(signal.SIGTERM,signal.SIG_IGN); time.sleep(30)",
            timeout=0.2, grace=0.1,
        )
        self.assertEqual(code, 75)
        self.assertIn("wall_timeout", output)
        self.assertLess(time.monotonic() - started, 3)

    def test_parent_signal_reaps_owned_child(self):
        with tempfile.TemporaryDirectory() as directory:
            pid_file = Path(directory) / "child.pid"
            child_program = (
                f"import os,time; open({str(pid_file)!r},'w').write(str(os.getpid())); time.sleep(30)"
            )
            script = (
                f"import sys,os; sys.path.insert(0,{str(Path(__file__).parent)!r}); "
                "import startup_policy; "
                f"sys.exit(startup_policy.run_policy([sys.executable,'-c',{child_program!r}],dict(os.environ)))"
            )
            parent = subprocess.Popen(
                [sys.executable, "-c", script], stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
            )
            try:
                deadline = time.monotonic() + 3
                while not pid_file.exists() and time.monotonic() < deadline:
                    time.sleep(0.02)
                self.assertTrue(pid_file.exists())
                child_pid = int(pid_file.read_text())
                parent.send_signal(signal.SIGTERM)
                self.assertEqual(parent.wait(timeout=3), 143)
                with self.assertRaises(ProcessLookupError):
                    os.kill(child_pid, 0)
            finally:
                if parent.poll() is None:
                    parent.kill()
                    parent.wait()

    def test_cli_rejects_non_loopback_before_connecting(self):
        env = dict(os.environ, DATABASE_HOST="example.invalid", DATABASE_PORT="5432",
                   DATABASE_USER="fixture", DATABASE_DBNAME="fixture",
                   DATABASE_PASSWORD="fixture")
        result = subprocess.run(
            [sys.executable, str(Path(policy.__file__)), "/not-called", __file__],
            env=env, capture_output=True, timeout=3,
        )
        self.assertEqual(result.returncode, 78)
        self.assertNotIn(b"fixture", result.stderr)

    def test_original_signal_handlers_are_restored(self):
        before = {s: signal.getsignal(s) for s in (signal.SIGTERM, signal.SIGINT)}
        self.run_command("pass")
        self.assertEqual(before, {s: signal.getsignal(s) for s in before})


if __name__ == "__main__":
    unittest.main()

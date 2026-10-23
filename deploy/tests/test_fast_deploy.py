"""No Docker, systemd, credentials or production connections used by this suite."""
from contextlib import redirect_stdout
import io
import os
import time
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import fast_deploy as d
import build_fast_release as b


class FakeHost:
    def __init__(self, root):
        self.c = {"pause_services": ["active.service", "inactive.service"], "app_container": "app"}
        self.marker, self.pending = root / "maintenance", root / "pending"
        self.events = []
        self.services = {"active.service": True, "inactive.service": False}
        self.current, self.running = "old", True
        self.busy = False
        self.bad_public = False
        self.bad_candidate = False
        self.stop_fail = False
        self.stop_hangs = False
        self.changed = False
        self.restore_fail = False
        self.pause_fail = False

    def service_state(self, name):
        return self.services[name]

    def pause(self, name):
        self.events.append("pause:" + name)
        d.require(not self.pause_fail, "pause failed")
        self.services[name] = False

    def restore(self, name):
        self.events.append("restore:" + name)
        d.require(not self.restore_fail, "restore failed")
        self.services[name] = True

    def gate(self, closed):
        self.events.append("close" if closed else "open")
        if closed:
            self.marker.touch()
        else:
            self.marker.unlink(missing_ok=True)

    def drain(self):
        self.events.append("drain")
        return not self.busy

    def unchanged(self, _):
        d.require(not self.changed, "plugin/schema drift")

    def stop(self, _):
        self.events.append("stop")
        if not self.stop_hangs:
            self.running = False
        d.require(not self.stop_fail, "shutdown incomplete")

    def start(self, target):
        self.current, self.running = target["image_id"], True
        self.events.append("start:" + self.current)
        d.require(not (self.bad_candidate and self.current == "new"), "candidate not ready")

    def inspect(self, _):
        return {"Image": self.current, "State": {"Running": self.running}}

    def verify(self, target, public=False):
        self.events.append("verify-public" if public else "verify")
        d.require(self.running and self.current == target["image_id"], "identity mismatch")
        if public and self.current == "new" and self.bad_public:
            raise RuntimeError("public 403")


class SwitchTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.attempt = self.root / "activate"
        self.attempt.mkdir()
        d.save(self.root / "prepared.json", {"previous": {"image_id": "old"}, "candidate": {"image_id": "new"}})
        d.save(self.attempt / "state.json", {"mode": "activate", "phase": "QUEUED"})
        self.h = FakeHost(self.root)
        self.s = d.Switch(self.root, self.attempt, self.h)
        self.s.preflight = lambda: None

    def assert_restored(self, phase, image="old"):
        self.assertEqual(self.s.s["phase"], phase)
        self.assertEqual(self.h.current, image)
        self.assertFalse(self.h.marker.exists())
        self.assertFalse(self.h.pending.exists())
        self.assertTrue(self.h.services["active.service"])
        self.assertFalse(self.h.services["inactive.service"])

    def test_success_only_starts_target_after_pause_and_drain(self):
        self.s.execute()
        self.assert_restored("ACCEPTED", "new")
        self.assertLess(self.h.events.index("pause:active.service"), self.h.events.index("drain"))
        self.assertLess(self.h.events.index("drain"), self.h.events.index("stop"))
        self.assertNotIn("restore:inactive.service", self.h.events)

    def test_busy_defers_without_stopping(self):
        self.h.busy = True
        self.s.execute()
        self.assert_restored("DEFERRED")
        self.assertNotIn("stop", self.h.events)

    def test_preflight_failure_has_no_side_effects(self):
        self.s.preflight = lambda: d.require(False, "stale backup")
        with self.assertRaises(RuntimeError):
            self.s.execute()
        self.assert_restored("PREFLIGHT_FAILED")
        self.assertEqual(self.h.events, [])

    def test_failed_candidate_rolls_back(self):
        self.h.bad_candidate = True
        with self.assertRaises(RuntimeError):
            self.s.execute()
        self.assert_restored("RECOVERED")
        self.assertIn("start:old", self.h.events)

    def test_public_failure_redrains_before_rollback(self):
        self.h.bad_public = True
        with self.assertRaises(RuntimeError):
            self.s.execute()
        self.assert_restored("RECOVERED")
        self.assertEqual(self.h.events.count("drain"), 2)

    def test_busy_after_public_open_does_not_stop_candidate(self):
        original_verify = self.h.verify
        def verify(target, public=False):
            if public and self.h.current == "new":
                self.h.busy = True
                raise RuntimeError("public failure")
            return original_verify(target, public)
        self.h.verify = verify
        with self.assertRaises(RuntimeError):
            self.s.execute()
        self.assertEqual(self.s.s["phase"], "NEEDS_ATTENTION")
        self.assertEqual(self.h.current, "new")
        self.assertEqual(self.h.events.count("stop"), 1)
        self.assertTrue(self.h.marker.exists())
        self.assertFalse(self.h.services["active.service"])

    def test_changed_plugin_or_schema_blocks_automatic_rollback(self):
        original_start = self.h.start
        def start(target):
            original_start(target)
            self.h.changed = True
            raise RuntimeError("new schema")
        self.h.start = start
        with self.assertRaises(RuntimeError):
            self.s.execute()
        self.assertEqual(self.s.s["phase"], "NEEDS_ATTENTION")
        self.assertNotIn("start:old", self.h.events)
        self.assertTrue(self.h.pending.exists())

    def test_unclean_completed_stop_restarts_original(self):
        self.h.stop_fail = True
        with self.assertRaises(RuntimeError):
            self.s.execute()
        self.assert_restored("RECOVERED")
        self.assertNotIn("start:new", self.h.events)

    def test_unknown_stop_result_is_not_reopened(self):
        self.h.stop_fail = self.h.stop_hangs = True
        with self.assertRaises(RuntimeError):
            self.s.execute()
        self.assertEqual(self.s.s["phase"], "NEEDS_ATTENTION")
        self.assertNotIn("open", self.h.events)

    def test_resume_failure_keeps_pending_and_maintenance(self):
        self.h.restore_fail = True
        with self.assertRaises(RuntimeError):
            self.s.execute()
        self.assertEqual(self.s.s["phase"], "NEEDS_ATTENTION")
        self.assertTrue(self.h.pending.exists())
        self.assertTrue(self.h.marker.exists())

    def test_failed_pause_never_stops_application(self):
        self.h.pause_fail = True
        with self.assertRaises(RuntimeError):
            self.s.execute()
        self.assertNotIn("stop", self.h.events)
        self.assertEqual(self.s.s["phase"], "NEEDS_ATTENTION")

    def test_stop_post_recovers_after_main_process_disappears(self):
        d.atomic(self.h.pending, str(self.attempt).encode())
        self.s.state("STARTING", touched=True, stop_started=True, stop_completed=True,
                     services={"active.service": True, "inactive.service": False})
        self.h.running = False
        self.h.services["active.service"] = False
        self.s = d.Switch(self.root, self.attempt, self.h)
        self.s.recover()
        self.assert_restored("RECOVERED")

    def test_stop_post_does_not_repeat_accepted_release(self):
        self.s.execute()
        events = list(self.h.events)
        self.s.recover()
        self.assertEqual(events, self.h.events)

    def test_manual_rollback_uses_same_drain_flow(self):
        self.s.s["mode"] = "rollback"
        d.save(self.attempt / "state.json", self.s.s)
        self.h.current = "new"
        self.s = d.Switch(self.root, self.attempt, self.h)
        self.s.preflight = lambda: None
        self.s.execute()
        self.assert_restored("ROLLED_BACK")


class UtilityTests(unittest.TestCase):
    def test_binary_backup_stream_never_includes_stderr(self):
        with tempfile.TemporaryFile() as dump, \
             patch.object(d.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, None, b"warning")) as run:
            d.run("docker", "exec", "postgres", "pg_dump", stdout=dump)
            self.assertIs(run.call_args.kwargs["stdout"], dump)
            self.assertEqual(run.call_args.kwargs["stderr"], subprocess.PIPE)

    def test_transient_plugin_error_does_not_hide_contract_changes(self):
        old = [{"state": "enabled", "binary_sha256": "same", "config_sha256": "same", "bindings": []}]
        unhealthy = [{**old[0], "state": "error"}]
        self.assertEqual(d.plugin_contract(old), d.plugin_contract(unhealthy))
        unhealthy[0]["binary_sha256"] = "different"
        self.assertNotEqual(d.plugin_contract(old), d.plugin_contract(unhealthy))

    def test_deployment_lock_is_exclusive(self):
        with tempfile.TemporaryDirectory() as root:
            with d.locked({"deploy_dir": root}):
                with self.assertRaises(BlockingIOError):
                    with d.locked({"deploy_dir": root}):
                        self.fail("second deployment acquired the lock")

    def test_pending_compose_changes_are_rejected(self):
        host = d.Host(d.read(Path(d.__file__).with_name("fast-deploy.example.json")))
        with patch.object(host, "compose", return_value=b"sub2api newhash\n"), \
             patch.object(host, "inspect", return_value={"Config": {"Labels": {"com.docker.compose.config-hash": "oldhash"}}}):
            with self.assertRaises(RuntimeError):
                host.check_applied_compose()

    def test_atomic_image_update_preserves_every_other_byte(self):
        old = b"# config\r\nPASSWORD=private\r\nSUB2API_IMAGE=old:v1\r\nOTHER=1\r\n"
        result = d.image_env(old, "new:v2")
        self.assertEqual(result, old.replace(b"old:v1", b"new:v2"))
        for invalid in (b"# SUB2API_IMAGE=old\n", b"SUB2API_IMAGE=a\nSUB2API_IMAGE=b\n"):
            with self.assertRaises(RuntimeError):
                d.image_env(invalid, "new")

    def test_atomic_write_preserves_mode_and_rejects_symlink(self):
        with tempfile.TemporaryDirectory() as root:
            path = Path(root) / "env"
            d.atomic(path, b"before", 0o640)
            d.atomic(path, b"after")
            self.assertEqual(path.read_bytes(), b"after")
            self.assertEqual(path.stat().st_mode & 0o777, 0o640)
            link = Path(root) / "link"
            link.symlink_to(path)
            with self.assertRaises(RuntimeError):
                d.atomic(link, b"bad")

    def test_contract_blocks_migrations_plugins_and_runtime(self):
        for path in ("backend/migrations/new.sql", "backend/internal/repository/migrations_runner_alias.go",
                     "backend/pkg/pluginapi/v1/plugin.proto", "deploy/codex-lb-cookie-pin/VERSION",
                     "deploy/auth-browser/launch.sh", "Dockerfile", "backend/go.mod"):
            with self.subTest(path=path), patch.object(b, "git", return_value=path):
                with self.assertRaises(RuntimeError):
                    b.check_contract("repo", "old", "new")
        with patch.object(b, "git", return_value="frontend/src/test.vue\nbackend/internal/service/model_plaza_service.go"):
            b.check_contract("repo", "old", "new")

    def test_build_version_preserves_plugin_semver_compatibility(self):
        for source, expected in (("0.3.0+r17bi\n", "0.3.0+r17bi.fast."),
                                 ("v0.3.1-rc.1", "0.3.1-rc.1+fast.")):
            self.assertEqual(b.deployment_version(source, "a" * 40), expected + "a" * 12)
        for invalid in ("deploy.abc", "01.2.3", "0.3.0\nother"):
            with self.assertRaises(RuntimeError):
                b.deployment_version(invalid, "a" * 40)

    def test_special_build_review_is_exact_and_only_allows_go_patch(self):
        paths = ["Dockerfile", "backend/go.mod", "deploy/handoff_deploy.py"]
        review = {"status": "approved", "source": "new", "previous_source": "old",
                  "changed_paths": paths, "reviewed_by": "fixture reviewer", "evidence": "fixture only"}
        files = {"old:backend/go.mod": b"module example\n\ngo 1.27.2\n",
                 "new:backend/go.mod": b"module example\n\ngo 1.27.3\n",
                 "old:Dockerfile": b"ARG GOLANG_IMAGE=golang:1.27.2-alpine\nFROM old-runtime\n",
                 "new:Dockerfile": b"ARG GOLANG_IMAGE=golang:1.27.3-alpine\nFROM old-runtime\n"}
        with patch.object(b, "git", return_value="\n".join(paths)), \
             patch.object(b, "run", side_effect=lambda *args: files[args[-1]]):
            b.check_contract("repo", "old", "new", review)
            for change in ({"status": "pending"}, {"source": "other"}, {"previous_source": "other"},
                           {"changed_paths": paths[:-1]}, {"reviewed_by": ""}, {"evidence": ""}):
                with self.subTest(change=change), self.assertRaises(RuntimeError):
                    b.check_contract("repo", "old", "new", {**review, **change})
            for path, data in (("new:backend/go.mod", b"module replaced\n\ngo 1.27.3\n"),
                               ("new:backend/go.mod", b"module example\n\ngo 1.28.0\n"),
                               ("new:backend/go.mod", b"module example\n\ngo 1.27.1\n"),
                               ("new:Dockerfile", files["new:Dockerfile"] + b"RUN unsafe\n")):
                with self.subTest(path=path, data=data), patch.dict(files, {path: data}), self.assertRaises(RuntimeError):
                    b.check_contract("repo", "old", "new", review)
        for path in ("backend/migrations/new.sql", "backend/go.sum", "backend/pkg/pluginapi/new.go",
                     "deploy/codex-lb-cookie-pin/VERSION"):
            changed = paths + [path]
            with patch.object(b, "git", return_value="\n".join(changed)), self.assertRaises(RuntimeError):
                b.check_contract("repo", "old", "new", {**review, "changed_paths": changed})

    def test_app_facts_ignores_volatile_version_log_timestamp(self):
        config = d.read(Path(d.__file__).with_name("fast-deploy.example.json"))
        host = d.Host(config)
        inspect = {"State": {"Running": True, "Restarting": False}, "Image": "sha256:image"}
        identity = "Sub2API v0.2.17+r17.1.fast.abc (commit: " + "a" * 40 + ", built: 2026-10-23T00:00:00Z)"
        versions = [(timestamp + '\tINFO\tstdlog\t' + identity + '\t{"legacy_stdlog":true}\n').encode()
                    for timestamp in ("2026-10-24T03:04:36Z", "2026-10-24T03:14:36Z")]
        with patch.object(host, "inspect", return_value=inspect), patch.object(
                d, "run", side_effect=(b"binary  /app/sub2api\n", versions[0],
                                       b"binary  /app/sub2api\n", versions[1])):
            facts = host.app_facts()
            self.assertEqual(facts, host.app_facts())
        self.assertEqual(facts, {"image_id": "sha256:image", "binary_sha256": "binary", "version": identity})

    def test_app_facts_preserves_real_version_identity_changes(self):
        host = d.Host(d.read(Path(d.__file__).with_name("fast-deploy.example.json")))
        inspect = {"State": {"Running": True, "Restarting": False}, "Image": "sha256:image"}
        identity = "Sub2API 0.3.0+r17bi.fast.abc (commit: " + "a" * 40 + ", built: 2026-10-23T00:00:00Z)"
        for changed in (identity.replace("r17bi", "r17bj"), identity.replace("a" * 40, "b" * 40),
                        identity.replace("00:00:00Z", "01:00:00Z")):
            with self.subTest(identity=changed), patch.object(host, "inspect", return_value=inspect), patch.object(
                    d, "run", side_effect=(b"binary  /app/sub2api\n", identity.encode(),
                                           b"binary  /app/sub2api\n", changed.encode())):
                self.assertNotEqual(host.app_facts(), host.app_facts())

    def test_app_facts_rejects_missing_or_incomplete_identity(self):
        host = d.Host(d.read(Path(d.__file__).with_name("fast-deploy.example.json")))
        inspect = {"State": {"Running": True, "Restarting": False}, "Image": "sha256:image"}
        for output in (b"", b"startup failed", b"Sub2API 0.2.17 (commit: abc, built: now)"):
            with self.subTest(output=output), patch.object(host, "inspect", return_value=inspect), patch.object(
                    d, "run", side_effect=(b"binary  /app/sub2api\n", output)), self.assertRaises(RuntimeError):
                host.app_facts()

    def test_restricted_guard_preserves_denial_when_open(self):
        config = d.read(Path(d.__file__).with_name("fast-deploy.example.json"))
        with tempfile.TemporaryDirectory() as root:
            config.update(deploy_dir=root, maintenance_file=root + "/maintenance")
            host = d.Host(config)
            with patch.object(host, "curl") as curl:
                host.gate(True)
                self.assertEqual(curl.call_args.kwargs["expected"], 503)
                host.gate(False)
                curl.assert_called_with("http://127.0.0.1:18080", "/health", expected=403)
        config["guard_checks"][0]["url"] = "http://remote.test/health"
        with self.assertRaises(RuntimeError):
            d.validate_config(config)

    def test_html_parser_finds_assets_with_different_quote_styles(self):
        html = "<script src='/assets/app.js'></script><link rel='stylesheet' href='/assets/app.css'><script src='https://elsewhere/a.js'></script>"
        self.assertEqual(d.EntryAssets(html).paths, {"/assets/app.js", "/assets/app.css"})

    def test_example_requires_secure_origins_and_short_drain(self):
        config = d.read(Path(d.__file__).with_name("fast-deploy.example.json"))
        d.validate_config(config)
        for key, value in (("public_urls", ["http://example.test"]), ("drain_seconds", 300),
                           ("local_url", "http://remote.test:8080"), ("previous_has_readyz", "auto")):
            with self.subTest(key=key), self.assertRaises(RuntimeError):
                d.validate_config({**config, key: value})

    def test_curl_is_identical_client_with_tls_and_private_loopback_header(self):
        host = d.Host(d.read(Path(d.__file__).with_name("fast-deploy.example.json")))
        with patch.object(d, "run", return_value=b'{"status":"ok"}\n200') as run:
            host.curl(host.c["public_urls"][0], "/health")
            public = run.call_args.args
            host.curl(host.c["local_url"], "/health", admin=True)
            local = run.call_args.args
        self.assertEqual(public[0:2], ("curl", "-q"))
        self.assertNotIn("-k", public)
        self.assertNotIn("--insecure", public)
        self.assertIn("@" + host.c["admin_header_file"], local)
        with self.assertRaises(RuntimeError):
            host.curl(host.c["public_urls"][0], "/health", admin=True)

    def test_curl_403_is_not_accepted_as_health(self):
        host = d.Host(d.read(Path(d.__file__).with_name("fast-deploy.example.json")))
        with patch.object(d, "run", return_value=b"Forbidden\n403"), self.assertRaises(RuntimeError):
            host.curl(host.c["public_urls"][0], "/health")

    def test_compose_drops_shell_business_overrides(self):
        host = d.Host(d.read(Path(d.__file__).with_name("fast-deploy.example.json")))
        result = subprocess.CompletedProcess([], 0, b"{}", b"")
        with patch.dict(os.environ, {"SUB2API_IMAGE": "bad", "DATABASE_HOST": "bad", "COMPOSE_FILE": "bad"}), \
             patch.object(d.subprocess, "run", return_value=result) as run:
            host.compose("config", "--format", "json")
            self.assertNotIn("DATABASE_HOST", run.call_args.kwargs["env"])
            self.assertNotIn("SUB2API_IMAGE", run.call_args.kwargs["env"])

    def test_data_identity_detects_restart_even_same_container_id(self):
        host = d.Host(d.read(Path(d.__file__).with_name("fast-deploy.example.json")))
        info = {"Id": "same", "Image": "same-image", "State": {"StartedAt": "before", "Health": {"Status": "healthy"}}}
        with patch.object(host, "inspect", return_value=info):
            before = host.data_identity()
            info["State"]["StartedAt"] = "after"
            self.assertNotEqual(before, host.data_identity())

    def test_live_direct_connection_blocks_zero_metrics(self):
        host = d.Host(d.read(Path(d.__file__).with_name("fast-deploy.example.json")))
        host.c["drain_seconds"] = 1
        metrics = {"enabled": True, "platform": {}, "user": {}}
        tcp = b"0: 00000000:1F90 00000000:FFFF 01 rest\n"
        with patch.object(host, "api", return_value=metrics), patch.object(d, "run", return_value=tcp):
            self.assertFalse(host.drain())

    def test_zero_connections_and_metrics_pass_three_checks(self):
        host = d.Host(d.read(Path(d.__file__).with_name("fast-deploy.example.json")))
        metrics = {"enabled": True, "platform": {}, "user": {}}
        with patch.object(host, "api", return_value=metrics) as api, patch.object(d, "run", return_value=b""):
            self.assertTrue(host.drain())
            self.assertEqual(api.call_count, 6)

    def test_probe_timeout_defers(self):
        host = d.Host(d.read(Path(d.__file__).with_name("fast-deploy.example.json")))
        with patch.object(host, "api", side_effect=subprocess.TimeoutExpired("curl", 1)):
            self.assertFalse(host.drain())

    def test_bootstrap_health_never_becomes_candidate_readiness_fallback(self):
        host = d.Host(d.read(Path(d.__file__).with_name("fast-deploy.example.json")))
        target = {"image_id": "id", "binary_sha256": "sha", "source": "commit", "assets": {}, "has_readyz": False}
        def response(origin, path, **kwargs):
            if path == "/readyz":
                raise RuntimeError("old binary has no readiness")
            return b'{"status":"ok"}'
        with patch.object(host, "app_facts", return_value={"image_id": "id", "binary_sha256": "sha", "version": "commit"}), \
             patch.object(host, "curl", side_effect=response) as curl, patch.object(host, "plugins", return_value=[]), \
             patch.object(host, "api"), patch.object(host, "assets"):
            host.verify(target)
            self.assertNotIn("/readyz", [call.args[1] for call in curl.call_args_list])
            with self.assertRaises(RuntimeError):
                host.verify({**target, "has_readyz": True})

    def test_evidence_cannot_be_missing_or_for_another_image(self):
        with tempfile.TemporaryDirectory() as root:
            root = Path(root)
            artifact = {k: k for k in ("source", "previous_source", "image_id", "base_image_id", "binary_sha256")}
            d.save(root / "config.json", {})
            receipt = {**artifact, "status": "passed", "config_sha256": d.digest(root / "config.json"), "plugins": [], "checks": {}}
            host = unittest.mock.Mock()
            host.plugins.return_value = []
            d.save(root / "evidence.json", receipt)
            with self.assertRaises(KeyError):
                d.evidence(root, artifact, host)
            receipt["image_id"] = "other"
            d.save(root / "evidence.json", receipt)
            with self.assertRaises(RuntimeError):
                d.evidence(root, artifact, host)


class StatusTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        d.save(self.root / "config.json", {"backup_max_age_seconds": 7200})
        d.save(self.root / "prepared.json", {
            "candidate": {"source": "a" * 40, "image_id": "sha256:candidate"}, "backup_at": 10000,
        })

    def status(self, now):
        before = {p: p.read_bytes() for p in self.root.rglob("*") if p.is_file()}
        output = io.StringIO()
        with patch.object(sys, "argv", ["fast_deploy.py", "status", "--release", str(self.root)]), \
             patch.object(d.time, "time", return_value=now), patch.object(d.os, "umask"), \
             patch.object(d, "run", side_effect=AssertionError("status must not run commands")), \
             patch.object(d, "Host", side_effect=AssertionError("status must not inspect production")), \
             redirect_stdout(output):
            d.main()
        self.assertEqual(before, {p: p.read_bytes() for p in self.root.rglob("*") if p.is_file()})
        return output.getvalue()

    def test_expired_material_is_not_reported_as_ready(self):
        result = self.status(17201)
        self.assertIn("BACKUP_WINDOW=EXPIRED", result)
        self.assertNotIn("PREPARED source=", result)

    def test_current_window_is_only_a_local_snapshot(self):
        result = self.status(10001)
        self.assertIn("PREPARATION_SNAPSHOT source=" + "a" * 40, result)
        self.assertIn("BACKUP_WINDOW=CURRENT", result)
        self.assertIn("expires_at=1970-01-01T04:46:40Z", result)
        self.assertIn("LIVE_STATE_NOT_CHECKED", result)

    def test_future_backup_timestamp_requires_clock_review(self):
        self.assertIn("BACKUP_WINDOW=CLOCK_INVALID", self.status(9999))

    def test_exact_expiry_matches_switch_preflight(self):
        self.assertIn("BACKUP_WINDOW=CURRENT", self.status(17200))

    def test_uses_configured_window(self):
        d.save(self.root / "config.json", {"backup_max_age_seconds": 60})
        self.assertIn("BACKUP_WINDOW=EXPIRED", self.status(10061))

    def test_historical_attempt_results_survive_backup_expiry(self):
        for mode, phase in (("activate", "ACCEPTED"), ("rollback", "ROLLED_BACK")):
            (self.root / mode).mkdir()
            d.save(self.root / mode / "state.json", {"mode": mode, "phase": phase})
        result = self.status(17201)
        for expected in ("BACKUP_WINDOW=EXPIRED", "ACCEPTED", "ROLLED_BACK"):
            self.assertIn(expected, result)

    def test_unprepared_release_has_no_backup_window(self):
        (self.root / "prepared.json").unlink()
        result = self.status(10001)
        self.assertIn("NOT_PREPARED", result)
        self.assertNotIn("BACKUP_WINDOW=", result)


class PreflightTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        d.atomic(self.root / "before.env", b"SUB2API_IMAGE=old\nSECRET=same\n")
        d.atomic(self.root / "current.env", (self.root / "before.env").read_bytes())
        d.atomic(self.root / "postgres.dump", b"backup")
        d.save(self.root / "artifact.json", {})
        self.p = {"previous": {"image": "old", "image_id": "oldid"}, "candidate": {"image": "new", "image_id": "newid"},
                  "sealed_files": {"before.env": d.digest(self.root / "before.env")},
                  "backup_at": time.time(), "backup_sha256": d.digest(self.root / "postgres.dump")}
        d.save(self.root / "prepared.json", self.p)
        self.attempt = self.root / "activate"
        self.attempt.mkdir()
        d.save(self.attempt / "state.json", {"mode": "activate", "phase": "QUEUED"})
        self.h = unittest.mock.Mock()
        self.h.c = {"backup_max_age_seconds": 7200, "env_file": str(self.root / "current.env")}
        self.h.marker, self.h.pending = self.root / "maintenance", self.root / "pending"
        self.h.inspect.side_effect = lambda name, image=False: {"Id": name + "id"}
        self.s = d.Switch(self.root, self.attempt, self.h)

    def test_preflight_checks_all_before_any_pause(self):
        with patch.object(d, "evidence"):
            self.s.preflight()
        self.h.verify.assert_called_once_with(self.p["previous"], public=True)
        self.h.unchanged.assert_called_once()
        self.h.pause.assert_not_called()
        self.h.stop.assert_not_called()

    def test_stale_backup_blocks_activation(self):
        self.s.p["backup_at"] -= 7201
        with self.assertRaisesRegex(RuntimeError, "backup expired"):
            self.s.preflight()
        self.h.verify.assert_not_called()

    def test_changed_sealed_file_blocks_activation(self):
        d.atomic(self.root / "before.env", b"modified")
        with self.assertRaisesRegex(RuntimeError, "prepared file changed"):
            self.s.preflight()

    def test_changed_backup_blocks_activation(self):
        d.atomic(self.root / "postgres.dump", b"damaged")
        with self.assertRaisesRegex(RuntimeError, "backup changed"):
            self.s.preflight()

    def test_retaged_image_blocks_activation(self):
        self.h.inspect.side_effect = lambda *a, **k: {"Id": "changed"}
        with patch.object(d, "evidence"), self.assertRaisesRegex(RuntimeError, "image tag changed"):
            self.s.preflight()

    def test_launch_is_detached_has_stop_post_and_cannot_repeat(self):
        config = d.read(Path(d.__file__).with_name("fast-deploy.example.json"))
        config.update(deploy_dir=str(self.root), maintenance_file=str(self.root / "maintenance"))
        d.save(self.root / "config.json", config)
        tools = self.root / "tools"
        tools.mkdir()
        d.atomic(tools / "fast_deploy.py", b"# frozen runner")
        self.p["sealed_files"]["tools/fast_deploy.py"] = d.digest(tools / "fast_deploy.py")
        d.save(self.root / "prepared.json", self.p)
        # The setUp attempt is a fixture only; launch must start with no attempt.
        (self.attempt / "state.json").unlink()
        self.attempt.rmdir()
        with patch.object(d, "run", return_value=b"") as run:
            d.launch(self.root, "activate")
            args = run.call_args.args
            self.assertEqual(args[0], "systemd-run")
            self.assertTrue(any("ExecStopPost=" in s and "_recover" in s for s in args))
            self.assertIn("--property=TimeoutStopSec=infinity", args)
            self.assertEqual(d.read(self.attempt / "state.json")["phase"], "QUEUED")
            with self.assertRaises(RuntimeError):
                d.launch(self.root, "activate")
            self.assertEqual(run.call_count, 1)


if __name__ == "__main__":
    unittest.main()

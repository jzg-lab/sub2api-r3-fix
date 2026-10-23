import json
import os
import http.client
from contextlib import ExitStack
from pathlib import Path
import socket
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from unittest.mock import Mock, patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import fast_deploy as d
import managed_deploy as m
import verify_fast_release as v


class ManagedDeploymentTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve()
        self.state = self.root / "state"
        self.state.mkdir(mode=0o700)
        self.config = self.root / "config.json"
        self.c = {"state_dir": str(self.state), "candidates_file": str(self.root / "candidates.json"),
                  "deployment_config": str(self.root / "deployment.json"), "repo": str(self.root / "repo"),
                  "min_free_bytes": 0, "min_available_memory_bytes": 0, "node_image": "node-pinned", "go_image": "go-pinned"}
        d.save(self.config, self.c)
        self.candidate = {"id": "release-1", "version": "v0.2.17+r17.1", "source": "b" * 40,
                          "previous_source": "a" * 40, "channel": "ordinary", "summary": "Reviewed fix", "review": "No data format changes; regression checked"}
        d.save(self.root / "candidates.json", [self.candidate])
        config = d.read(Path(d.__file__).with_name("fast-deploy.example.json"))
        config.update(deploy_dir=str(self.root), maintenance_file=str(self.root / "maintenance"))
        d.save(self.root / "deployment.json", config)
        self.controller = m.Controller(self.config)

    def queue(self):
        with patch.object(d, "run", return_value=b"") as run:
            result = self.controller.command("prepare", "release-1")
            args = run.call_args.args
        self.assertEqual(args[0], "systemd-run")
        self.assertTrue(any("ExecStopPost=" in x and "interrupted" in x for x in args))
        self.assertEqual(result["job"]["phase"], "QUEUED")
        self.assertNotIn("activate", args)
        return self.controller.latest()

    def prepared(self):
        job = self.queue()
        release = job / "release"
        release.mkdir()
        d.save(release / "prepared.json", {"backup_at": time.time()})
        d.save(release / "config.json", {"backup_max_age_seconds": 7200})
        self.controller.state(job, "PREPARED")
        return job

    def test_repeated_prepare_attaches_to_one_job(self):
        first = self.queue()
        with patch.object(d, "run", side_effect=AssertionError("must not launch twice")):
            result = self.controller.command("prepare", "release-1")
        self.assertEqual(result["job"]["id"], first.name)

    def test_removed_parallel_mode_cannot_be_interpreted_as_maintenance(self):
        d.save(self.config, {**self.c, "mode": "blue-green"})
        with self.assertRaisesRegex(RuntimeError, "unsupported deployment"):
            m.Controller(self.config)
        d.save(self.config, self.c)
        job = self.prepared()
        self.controller.state(job, "PREPARED", mode="blue-green")
        with self.assertRaisesRegex(RuntimeError, "unsupported saved"):
            self.controller.snapshot()

    def test_controller_lock_serializes_and_rejects_replaced_inode(self):
        with self.controller.lock():
            with self.assertRaises(BlockingIOError):
                with self.controller.lock():
                    self.fail("lock allowed a second owner")
        path = self.state / "control.lock"
        real_open = os.open
        def replace_before_open(target, *args, **kwargs):
            path.rename(path.with_suffix(".old"))
            # Path.touch uses the mocked os.open on Python 3.12 and newer.
            os.close(real_open(path, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600))
            return real_open(target, *args, **kwargs)
        with patch.object(os, "open", side_effect=replace_before_open):
            with self.assertRaisesRegex(RuntimeError, "lock replaced"):
                with self.controller.lock():
                    self.fail("replaced inode accepted")
        path.unlink()
        path.symlink_to(path.with_suffix(".old"))
        with self.assertRaisesRegex(RuntimeError, "unsafe controller lock"):
            with self.controller.lock():
                self.fail("symlink accepted")

    def test_special_and_unreviewed_candidates_cannot_prepare(self):
        for changes in ({"channel": "special"}, {"review": ""}, {"source": "main"}):
            d.save(self.root / "candidates.json", [{**self.candidate, **changes}])
            with self.assertRaises(RuntimeError), patch.object(d, "run", side_effect=AssertionError("must not launch")):
                self.controller.command("prepare", "release-1")
        self.assertIsNone(self.controller.latest())

    def test_unrecognized_commands_and_paths_never_launch(self):
        for action, identifier in (("shell", "release-1"), ("prepare", "../a"), ("prepare", "a;shutdown"), ("prepare", "unknown")):
            with self.subTest(action=action, identifier=identifier), self.assertRaises(RuntimeError):
                self.controller.command(action, identifier)

    def test_pending_recovery_blocks_new_preparation(self):
        (self.root / ".fast-deploy-pending").touch()
        with self.assertRaises(RuntimeError):
            self.controller.command("prepare", "release-1")

    def test_expiry_removes_mutation_actions_without_overwriting_history(self):
        job = self.prepared()
        self.assertEqual(self.controller.snapshot()["job"]["actions"], ["activate"])
        d.save(job / "release/prepared.json", {"backup_at": time.time() - 7201})
        result = self.controller.snapshot()["job"]
        self.assertEqual(result["phase"], "EXPIRED")
        self.assertEqual(result["actions"], [])
        with self.assertRaises(RuntimeError):
            self.controller.command("activate", job.name)

    def test_activate_uses_sealed_runner_and_is_idempotent(self):
        job = self.prepared()
        def launch(*args, **kwargs):
            self.assertEqual(args[:2], ("/usr/bin/python3", str(job / "release/tools/fast_deploy.py")))
            attempt = job / "release/activate"
            attempt.mkdir()
            d.save(attempt / "state.json", {"phase": "ACCEPTED"})
        with patch.object(d, "run", side_effect=launch) as run:
            self.controller.command("activate", job.name)
            result = self.controller.command("activate", job.name)
            self.assertEqual(run.call_count, 1)
        self.assertEqual(result["job"]["phase"], "ACCEPTED")
        self.assertEqual(result["job"]["actions"], ["rollback"])

    def test_unknown_switch_result_blocks_another_job(self):
        job = self.prepared()
        self.controller.state(job, "NEEDS_ATTENTION")
        with self.assertRaises(RuntimeError):
            self.controller.command("prepare", "release-1")

    def test_controller_restart_recovers_persisted_job(self):
        job = self.prepared()
        self.assertEqual(m.Controller(self.config).snapshot()["job"]["id"], job.name)

    def test_auxiliary_recovery_is_visible_and_blocks_another_handoff(self):
        job = self.prepared()
        self.controller.state(job, "ACCEPTED", mode="handoff")
        d.save(job / "release/config.json", {"backup_max_age_seconds": 7200, "deploy_dir": str(self.root)})
        record = self.root / "handoff-active.json"
        d.save(record, {"pending_services": ["caller.service"]})
        state = self.controller.job_status(job)
        self.assertEqual(state["phase"], "ACCEPTED")
        self.assertEqual(state["error_code"], "AUXILIARY_SERVICES_PENDING")
        self.assertEqual(state["actions"], [])
        d.save(record, {"pending_services": []})
        state = self.controller.job_status(job)
        self.assertEqual(state["error_code"], "")
        self.assertEqual(state["actions"], ["rollback"])

    def test_previous_version_comes_only_from_matching_prepared_identity(self):
        job = self.prepared()
        source = self.candidate["previous_source"]
        for version in ("0.3.0+r17bi.fast.123abc", "v0.2.17+r17.1"):
            prepared = {"backup_at": time.time(), "previous": {"source": source,
                        "version": f"2026/10/22 01:00:00 Sub2API {version} (commit: {source}, built: now)\n"}}
            d.save(job / "release/prepared.json", prepared)
            self.assertEqual(self.controller.job_status(job)["previous_version"], "v" + version.removeprefix("v"))
            prepared["previous"]["source"] = "c" * 40
            d.save(job / "release/prepared.json", prepared)
            self.assertEqual(self.controller.job_status(job)["previous_version"], "")
        d.save(job / "release/prepared.json", {"backup_at": time.time()})
        self.assertEqual(self.controller.job_status(job)["previous_version"], "")

    def test_live_handoff_retirement_is_visible_and_blocks_mutations(self):
        job = self.prepared()
        self.controller.state(job, "ACCEPTED", mode="handoff")
        d.save(job / "release/config.json", {"backup_max_age_seconds": 7200, "deploy_dir": str(self.root)})
        d.save(job / "release/prepared.json", {"backup_at": time.time(), "handoff": {"protocol": 2}})
        record = self.root / "handoff-active.json"
        d.save(record, {"retiring": {"slot": "blue"}, "pending_services": []})
        state = self.controller.job_status(job)
        self.assertEqual(state["phase"], "ACCEPTED")
        self.assertTrue(state["live_handoff"])
        self.assertEqual(state["error_code"], "PREVIOUS_REQUESTS_PENDING")
        self.assertEqual(state["actions"], [])
        with self.assertRaises(RuntimeError), patch.object(d, "run", side_effect=AssertionError("must not launch")):
            self.controller.command("prepare", "release-1")
        d.save(record, {"retiring": None, "pending_services": []})
        self.assertEqual(self.controller.job_status(job)["actions"], ["rollback"])

    def test_failed_new_preparation_preserves_previous_rollback_and_idempotency(self):
        accepted = self.prepared()
        self.controller.state(accepted, "ACCEPTED")
        newer = self.queue()
        self.assertIsNone(self.controller.snapshot()["rollback_job"])
        with self.assertRaises(RuntimeError):
            self.controller.command("rollback", accepted.name)
        self.controller.state(newer, "PREPARATION_FAILED")
        self.assertEqual(self.controller.snapshot()["rollback_job"]["id"], accepted.name)
        def launch(*args, **kwargs):
            self.assertEqual(args[2], "rollback")
            attempt = accepted / "release/rollback"
            attempt.mkdir()
            d.save(attempt / "state.json", {"phase": "ROLLED_BACK"})
        with patch.object(d, "run", side_effect=launch) as run:
            self.controller.command("rollback", accepted.name)
            result = self.controller.command("rollback", accepted.name)
        self.assertEqual(run.call_count, 1)
        self.assertEqual(result["job"]["phase"], "ROLLED_BACK")
        self.assertIsNone(result["rollback_job"])
        self.assertTrue((newer / "state.json").exists())

    def test_new_acceptance_supersedes_old_rollback_even_when_new_backup_expires(self):
        first = self.prepared()
        self.controller.state(first, "ACCEPTED")
        newer = self.prepared()
        self.controller.state(newer, "ACCEPTED")
        self.assertEqual(self.controller.snapshot()["rollback_job"]["id"], newer.name)
        d.save(newer / "release/prepared.json", {"backup_at": time.time() - 7201})
        self.assertIsNone(self.controller.snapshot()["rollback_job"])
        with self.assertRaises(RuntimeError):
            self.controller.command("rollback", first.name)

    def test_saved_rollback_expires_and_catalog_failure_does_not_hide_it(self):
        accepted = self.prepared()
        self.controller.state(accepted, "ACCEPTED")
        newer = self.queue()
        self.controller.state(newer, "PREPARATION_FAILED")
        with patch.object(self.controller, "catalog", side_effect=OSError()):
            self.assertEqual(self.controller.snapshot()["rollback_job"]["id"], accepted.name)
        d.save(accepted / "release/prepared.json", {"backup_at": time.time() - 7201})
        self.assertIsNone(self.controller.snapshot()["rollback_job"])

    def verified(self):
        job = self.prepared()
        (job / "build").mkdir()
        (job / "lab").mkdir()
        artifact = {"source": self.candidate["source"], "previous_source": self.candidate["previous_source"],
                    "image": "candidate", "image_id": "sha256:new", "base_image_id": "sha256:old", "binary_sha256": "binary"}
        d.save(job / "build/artifact.json", artifact)
        d.save(job / "lab/cleanup.json", {"status": "passed"})
        receipt = {**artifact, "status": "passed", "config_sha256": d.digest(job / "release/config.json"), "plugins": [], "checks": {}}
        for name in ("runtime_uid_paths", "plugin_rpc_existing_config", "startup_readiness", "compatible_rollback", "direct_callers_inventory", "nginx_guard"):
            d.save(job / "release" / (name + ".log"), {"status": "passed"})
            receipt["checks"][name] = {"status": "passed", "log": name + ".log", "sha256": d.digest(job / "release" / (name + ".log"))}
        d.save(job / "release/evidence.json", receipt)
        host = Mock()
        host.plugins.return_value = []
        host.inspect.return_value = {"Id": "sha256:new"}
        context = {"candidate": self.candidate, "inputs": "unchanged"}
        with patch.object(self.controller, "verification_context", return_value=context):
            self.controller.seal_verification(job, context, host)
        return job, context, host

    def test_reuse_copies_only_verified_material_not_old_backup_or_attempt(self):
        old, context, host = self.verified()
        d.save(old / "release/postgres.dump", {"never": "copy"})
        self.controller.state(old, "DEFERRED")
        newer = self.queue()
        self.assertTrue(self.controller.reuse_verified(newer, context, host))
        self.assertTrue(self.controller.job_status(newer)["reused"])
        self.assertFalse((newer / "release/prepared.json").exists())
        self.assertFalse((newer / "release/postgres.dump").exists())
        self.assertFalse((newer / "release/activate").exists())
        d.evidence(newer / "release", d.read(newer / "build/artifact.json"), host)

    def test_cache_drift_and_tampered_or_missing_material_cause_full_rebuild(self):
        old, context, host = self.verified()
        self.controller.state(old, "EXPIRED")
        d.save(old / "release/prepared.json", {"backup_at": time.time() - 7201})
        newer = self.queue()
        self.assertFalse(self.controller.reuse_verified(newer, {**context, "inputs": "changed"}, host))
        host.inspect.return_value = {"Id": "sha256:changed"}
        self.assertFalse(self.controller.reuse_verified(newer, context, host))
        host.inspect.return_value = {"Id": "sha256:new"}
        log = old / "release/runtime_uid_paths.log"
        log.write_text("changed")
        self.assertFalse(self.controller.reuse_verified(newer, context, host))
        log.unlink()
        self.assertFalse(self.controller.reuse_verified(newer, context, host))
        self.assertFalse((newer / "build").exists())

    def test_expired_retry_skips_build_and_fixture_but_always_prepares_new_backup(self):
        old, context, host = self.verified()
        d.save(old / "release/prepared.json", {"backup_at": time.time() - 7201})
        newer = self.queue()
        host.marker, host.pending = self.root / "marker", self.root / "pending"
        host.app_facts.return_value = {"version": self.candidate["previous_source"], "image_id": "sha256:old"}
        read_text = Path.read_text
        def read(path, *args, **kwargs):
            return "MemAvailable: 10000000 kB\n" if str(path) == "/proc/meminfo" else read_text(path, *args, **kwargs)
        with ExitStack() as stack:
            stack.enter_context(patch.object(d, "Host", return_value=host))
            stack.enter_context(patch.object(Path, "read_text", autospec=True, side_effect=read))
            stack.enter_context(patch.object(m.build, "git", side_effect=["https://github.com/jzg-lab/sub2api-r3-fix.git", "frontend/src/main.ts", self.candidate["version"]]))
            stack.enter_context(patch.object(d, "run", return_value=b""))
            stack.enter_context(patch.object(self.controller, "verification_context", return_value=context))
            build = stack.enter_context(patch.object(m.build, "build"))
            verify = stack.enter_context(patch.object(v, "verify"))
            backup = stack.enter_context(patch.object(d, "prepare"))
            self.controller.prepare(newer.name)
            build.assert_not_called()
            verify.assert_not_called()
            backup.assert_called_once_with(newer / "release", Path(self.c["deployment_config"]), newer / "build/artifact.json")
        self.assertEqual(d.read(newer / "state.json")["phase"], "PREPARED")

    def test_verification_context_tracks_tools_configuration_plugins_and_data(self):
        job = self.queue()
        env = self.root / ".env"
        d.atomic(env, b"SUB2API_IMAGE=old\n")
        host = Mock()
        host.c = {"env_file": str(env)}
        host.app_facts.return_value = {"image_id": "old"}
        host.data_identity.return_value = {"postgres": "old"}
        host.migrations.return_value = {"001": "checksum"}
        host.config_hashes.return_value = {"config": "checksum"}
        host.compose_hash.return_value = "compose"
        with patch.object(v, "approved_inputs", return_value=({"package": "original"}, [])):
            before = self.controller.verification_context(job, host)
            host.data_identity.return_value = {"postgres": "restarted"}
            self.assertNotEqual(before, self.controller.verification_context(job, host))
            host.data_identity.return_value = {"postgres": "old"}
            d.atomic(env, b"SUB2API_IMAGE=other\n")
            self.assertNotEqual(before, self.controller.verification_context(job, host))
        self.assertEqual(set(before["tools"]), {"managed_deploy.py", "verify_fast_release.py", "build_fast_release.py", "fast_deploy.py", "handoff_deploy.py", "FastRuntime.Dockerfile"})

    def test_socket_restart_preserves_directory_and_reclaims_only_stale_socket(self):
        self.controller.c.update(socket_path=str(self.root / "control.sock"), socket_gid=os.getgid())
        path = Path(self.controller.c["socket_path"])
        inode = self.root.stat().st_ino
        stale = socket.socket(socket.AF_UNIX)
        stale.bind(str(path))
        stale.close()
        with m.controller_server(self.controller):
            with self.assertRaises(BlockingIOError):
                with m.controller_server(self.controller):
                    self.fail("duplicate controller")
        with m.controller_server(self.controller):
            self.assertEqual(self.root.stat().st_ino, inode)
        path.write_text("not a socket")
        with self.assertRaises(RuntimeError):
            with m.controller_server(self.controller):
                self.fail("must preserve unexpected path")
        self.assertEqual(path.read_text(), "not a socket")

    def test_actual_controller_crash_and_restart_answers_same_socket_path(self):
        self.controller.c.update(socket_path=str(self.root / "control.sock"), socket_gid=os.getgid())
        d.save(self.config, self.controller.c)
        script = "import managed_deploy as m, sys; c=m.Controller(sys.argv[1]);\nwith m.controller_server(c) as s: s.serve_forever()"
        def start():
            child = subprocess.Popen([sys.executable, "-c", script, str(self.config)], cwd=Path(m.__file__).parent,
                                     stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
            self.addCleanup(lambda: child.poll() is None and (child.kill(), child.wait()))
            deadline = time.monotonic() + 5
            while time.monotonic() < deadline:
                try:
                    connection = http.client.HTTPConnection("localhost", timeout=0.2)
                    connection.sock = socket.socket(socket.AF_UNIX)
                    connection.sock.settimeout(0.2)
                    connection.sock.connect(self.controller.c["socket_path"])
                    connection.request("GET", "/status")
                    response = connection.getresponse()
                    self.assertEqual(response.status, 200)
                    self.assertTrue(json.loads(response.read())["enabled"])
                    return child
                except OSError:
                    time.sleep(0.02)
                finally:
                    connection.close()
            child.kill()
            self.fail(child.communicate()[1].decode())
        inode = self.root.stat().st_ino
        first = start()
        first.kill()
        first.communicate()
        self.assertTrue(Path(self.controller.c["socket_path"]).exists())
        second = start()
        self.assertEqual(self.root.stat().st_ino, inode)
        second.kill()
        second.communicate()

    def test_live_listener_is_never_removed_even_without_controller_lock(self):
        self.controller.c.update(socket_path=str(self.root / "control.sock"), socket_gid=os.getgid())
        path = Path(self.controller.c["socket_path"])
        with m.socketserver.UnixStreamServer(str(path), m.Handler):
            inode = path.stat().st_ino
            with self.assertRaisesRegex(RuntimeError, "already listening"):
                with m.controller_server(self.controller):
                    self.fail("must not replace live listener")
            self.assertEqual(path.stat().st_ino, inode)

    def test_catalog_outage_keeps_results_and_compatible_rollback(self):
        job = self.prepared()
        with patch.object(self.controller, "catalog", side_effect=OSError("registry unavailable")):
            state = self.controller.snapshot()
            self.assertEqual(state["job"]["phase"], "PREPARED")
            self.assertEqual(state["job"]["actions"], [])
            self.assertEqual(state["catalog_error"], "CATALOG_UNAVAILABLE")
            with self.assertRaises(OSError), patch.object(d, "run") as run:
                self.controller.command("activate", job.name)
            run.assert_not_called()
            self.controller.state(job, "ACCEPTED")
            self.assertEqual(self.controller.snapshot()["job"]["actions"], ["rollback"])
            with patch.object(d, "run") as run:
                self.controller.command("rollback", job.name)
            self.assertEqual(run.call_args.args[2], "rollback")

    def test_changed_approval_blocks_activation(self):
        job = self.prepared()
        d.save(self.root / "candidates.json", [{**self.candidate, "source": "c" * 40}])
        with self.assertRaisesRegex(RuntimeError, "approval changed"), patch.object(d, "run") as run:
            self.controller.command("activate", job.name)
        run.assert_not_called()
        self.assertEqual(self.controller.snapshot()["job"]["phase"], "PREPARED")

    def test_github_catalog_is_bounded_validated_and_cached(self):
        self.controller.c["github_catalog"] = True
        response = Mock()
        response.geturl.return_value = m.CATALOG_URL
        response.read.return_value = json.dumps([self.candidate]).encode()
        response.__enter__ = Mock(return_value=response)
        response.__exit__ = Mock(return_value=False)
        with patch.object(m, "urlopen", return_value=response) as request:
            self.assertEqual(self.controller.catalog(refresh=True), [self.candidate])
            self.controller.catalog()
            request.assert_called_once_with(m.CATALOG_URL, timeout=5)
            response.read.assert_called_once_with(131073)
            response.geturl.return_value = "https://example.invalid/catalog"
            with self.assertRaisesRegex(RuntimeError, "redirect"):
                self.controller.catalog(refresh=True)
            response.geturl.return_value = m.CATALOG_URL
            response.read.return_value = b" " * 131073
            with self.assertRaisesRegex(RuntimeError, "too large"):
                self.controller.catalog(refresh=True)
        self.assertEqual(d.read(self.root / "candidates.json"), [self.candidate])

    def test_baseline_drift_is_reported_as_blocked_without_building(self):
        job = self.queue()
        host = Mock()
        host.marker, host.pending = self.root / "marker", self.root / "pending"
        host.app_facts.return_value = {"version": "c" * 40, "image_id": "old"}
        with patch.object(d, "Host", return_value=host), patch.object(m.build, "build") as build:
            with self.assertRaisesRegex(m.PreparationBlocked, "BASELINE_CHANGED"):
                self.controller.prepare(job.name)
        build.assert_not_called()
        state = self.controller.snapshot()["job"]
        self.assertEqual(state["phase"], "BLOCKED")
        self.assertEqual(state["error_code"], "BASELINE_CHANGED")

    def test_http_protocol_accepts_only_fixed_actions_and_ids(self):
        path = str(self.root / "control.sock")
        with m.socketserver.UnixStreamServer(path, m.Handler) as server:
            server.control = Mock()
            server.control.snapshot.return_value = {"enabled": True, "candidates": []}
            server.control.command.return_value = {"enabled": True, "candidates": []}
            worker = threading.Thread(target=server.serve_forever)
            worker.start()
            try:
                def request(method, route, data=None):
                    connection = http.client.HTTPConnection("localhost", timeout=2)
                    import socket
                    connection.sock = socket.socket(socket.AF_UNIX)
                    connection.sock.settimeout(2)
                    connection.sock.connect(path)
                    payload = (data or "").encode()
                    request = (f"{method} {route} HTTP/1.0\r\nHost: localhost\r\nContent-Length: {len(payload)}\r\n\r\n").encode() + payload
                    try:
                        # Send the small test request together; invalid headers may be rejected before body reads.
                        connection.sock.sendall(request)
                        response = http.client.HTTPResponse(connection.sock)
                        response.begin()
                        return response.status, json.loads(response.read())
                    finally:
                        connection.close()
                self.assertEqual(request("GET", "/status?refresh=true")[0], 200)
                server.control.snapshot.assert_called_once_with(True)
                self.assertEqual(request("POST", "/prepare", '{"id":"release-1"}')[0], 200)
                server.control.command.assert_called_once_with("prepare", "release-1")
                for route, body in (("/shell", '{}'), ("/prepare", '{"id":"a","command":"restart"}'),
                                    ("/prepare", '[]'), ("/prepare", 'x' * 1025)):
                    self.assertEqual(request("POST", route, body)[0], 409)
                self.assertEqual(server.control.command.call_count, 1)
                server.control.command.side_effect = RuntimeError("private path and credentials")
                self.assertEqual(request("POST", "/activate", '{"id":"job-1"}'),
                                 (409, {"error": "DEPLOYMENT_CONFLICT"}))
            finally:
                server.shutdown()
                worker.join(timeout=2)

    def test_failed_verification_never_calls_production_prepare(self):
        job = self.queue()
        host = Mock()
        host.marker, host.pending = self.root / "marker", self.root / "pending"
        host.app_facts.return_value = {"version": "a" * 40, "image_id": "old"}
        original_read = Path.read_text
        def read(path, *args, **kwargs):
            if str(path) == "/proc/meminfo":
                return "MemAvailable: 100000 kB\n"
            return original_read(path, *args, **kwargs)
        def git(repo, *args):
            if args[0] == "remote": return "https://github.com/jzg-lab/sub2api-r3-fix.git"
            if args[0] == "show": return "0.2.17+r17.1"
            return ""
        with patch.object(d, "Host", return_value=host), patch.object(d, "run"), \
             patch.object(m.build, "git", side_effect=git), patch.object(m.build, "build"), \
             patch.object(m.build, "limited_build_options", return_value=[]), \
             patch.object(self.controller, "verification_context", return_value={}), \
             patch.object(Path, "read_text", read), patch.object(v, "verify", side_effect=RuntimeError("test failed")), \
             patch.object(d, "prepare") as prepare:
            with self.assertRaisesRegex(RuntimeError, "test failed"):
                self.controller.prepare(job.name)
            prepare.assert_not_called()
        self.assertEqual(self.controller.snapshot()["job"]["phase"], "PREPARATION_FAILED")
        host.pause.assert_not_called()
        host.gate.assert_not_called()
        host.stop.assert_not_called()

    def test_shared_catalog_is_not_accepted(self):
        self.config.chmod(0o644)
        with self.assertRaises(RuntimeError):
            m.Controller(self.config)


class BuildBudgetTests(unittest.TestCase):
    def check(self, *, memory="6442450944", cpu="200000 100000", swap="0", driver="docker"):
        responses = [b"unix:///var/run/docker.sock", b'{"OSType":"linux","CgroupVersion":"2"}',
                     b"\n".join(json.dumps(builder).encode() for builder in (
                         {"Name": "other", "Driver": "docker-container"},
                         {"Name": "default", "Driver": driver},
                         {"Name": "default", "Driver": driver})), m.build.BUILD_CGROUP.encode()]
        values = {"memory.max": memory, "cpu.max": cpu, "memory.swap.max": swap}
        with patch.dict(os.environ, {}, clear=True), patch.object(m.build, "run", side_effect=responses) as run, \
             patch.object(Path, "read_text", autospec=True, side_effect=lambda path: values[path.name]):
            result = m.build.limited_build_options(m.build.BUILD_CGROUP)
        self.assertEqual(run.call_args_list[2].args, ("docker", "buildx", "ls", "--format", "{{json .}}"))
        return result

    def test_budget_applies_to_buildkit_containers(self):
        self.assertEqual(self.check(), ["--builder", "default", "--cgroup-parent", m.build.BUILD_CGROUP])

    def test_unlimited_or_wrong_builder_is_rejected(self):
        for values in ({"memory": "max"}, {"memory": "9999999999"}, {"cpu": "max 100000"},
                       {"cpu": "300000 100000"}, {"swap": "max"}, {"driver": "docker-container"}):
            with self.subTest(values=values), self.assertRaises(RuntimeError):
                self.check(**values)

    def test_missing_slice_and_remote_override_are_rejected(self):
        with patch.dict(os.environ, {"DOCKER_HOST": "tcp://remote:2375"}), self.assertRaises(RuntimeError):
            m.build.limited_build_options(m.build.BUILD_CGROUP)
        with patch.dict(os.environ, {}, clear=True), patch.object(m.build, "run", side_effect=RuntimeError("missing slice")), self.assertRaises(RuntimeError):
            m.build.limited_build_options(m.build.BUILD_CGROUP)


class FixtureIsolationTests(unittest.TestCase):
    def test_caller_inventory_ignores_transient_deployment_jobs_only(self):
        host = Mock(c={"pause_services": ["caller.service"]})
        persistent = b"caller.service enabled -\nother.timer enabled -\n"

        def inventory(units):
            with patch.object(d, "run", side_effect=[units, b"FragmentPath=/etc/systemd/system/caller.service\n"]):
                return v.caller_inventory(host)

        baseline = inventory(persistent)
        self.assertEqual(inventory(persistent + b"sub2api-prepare-123.service transient -\n"), baseline)
        self.assertNotEqual(inventory(persistent + b"new-caller.timer enabled -\n"), baseline)

    def test_runtime_uid_uses_the_application_process_not_docker_exec(self):
        with patch.object(d, "run", return_value=b"Name:\tsub2api\nUid:\t1000\t1000\t1000\t1000\n") as run:
            v.check_runtime_uid("fixture")
            self.assertEqual(run.call_args.args[-1], "/proc/1/status")
        with patch.object(d, "run", return_value=b"Uid:\t0\t0\t0\t0\n"), self.assertRaises(RuntimeError):
            v.check_runtime_uid("fixture")

    def test_plugin_config_endpoint_returns_raw_json_not_an_api_envelope(self):
        with tempfile.TemporaryDirectory() as root:
            fixture = v.Fixture.__new__(v.Fixture)
            fixture.data, fixture.names = Path(root), {"app": "fixture-app"}
            fixture.h = Mock()
            fixture.h.c = {"local_url": "http://127.0.0.1:17880"}
            with patch.object(d, "run", return_value=b'{"enabled":true}\n200'):
                self.assertEqual(fixture.api("/admin/plugins/1/config", "PUT", raw=True), {"enabled": True})
                with self.assertRaises(RuntimeError):
                    fixture.api("/admin/plugins/1/status")
            with patch.object(d, "run", return_value=b'{"code":0,"data":{"access_token":"fixture"}}\n200'):
                self.assertEqual(fixture.api("/auth/login"), {"access_token": "fixture"})

    def test_fixture_has_no_production_mounts_ports_or_outbound_network(self):
        with tempfile.TemporaryDirectory() as root:
            job = Path(root) / "job-test"
            job.mkdir()
            host = Mock()
            host.c = d.read(Path(d.__file__).with_name("fast-deploy.example.json"))
            host.inspect.return_value = {"Image": "sha256:fixture"}
            with patch.object(os, "chown"):
                fixture = v.Fixture(job, host, {"base_image_id": "sha256:old"}, {})
            compose = fixture.compose
            self.assertTrue(compose["networks"]["default"]["internal"])
            for service in compose["services"].values():
                self.assertNotIn("ports", service)
                self.assertTrue(service["container_name"].startswith("sub2api-check-job-test-"))
            self.assertEqual(compose["services"]["sub2api"]["volumes"], [str(job / "lab/data") + ":/app/data"])
            with patch.object(d, "run", return_value=b"existing-container"), patch.object(fixture.h, "compose") as command:
                with self.assertRaises(RuntimeError):
                    fixture.setup()
                fixture.cleanup()
                command.assert_not_called()

    def test_changed_plugin_fixture_rejected_before_creation(self):
        with tempfile.TemporaryDirectory() as root:
            path = Path(root) / "manifest.json"
            d.save(path, {"production_plugins": []})
            host = Mock()
            host.plugins.return_value = [{"state": "enabled", "id": 1}]
            with patch.object(v, "Fixture") as fixture, self.assertRaises(RuntimeError):
                v.verify(Path(root), {"verification_manifest": str(path)}, host)
            fixture.assert_not_called()


if __name__ == "__main__":
    unittest.main()

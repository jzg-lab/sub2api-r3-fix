import copy
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import fast_deploy as d
import handoff_deploy as h


class FakeHost:
    def __init__(self, root):
        self.c = {"deploy_dir": str(root), "pause_services": ["caller.service", "unused.service"],
                  "drain_seconds": 2, "ready_seconds": 2}
        self.marker, self.pending = root / "maintenance", root / "pending"
        self.states = {"blue": "active", "green": "standby"}
        self.boots = {"blue": "a" * 32, "green": "b" * 32}
        self.services = {"caller.service": True, "unused.service": False}
        self.selected_slot = "blue"
        self.events = []
        self.fail = None
        self.settings = "unchanged"
        self.runtime_checks = []

    def event(self, value):
        self.events.append(value)
        if self.fail == value:
            self.fail = None
            raise RuntimeError("injected")

    def selected(self):
        return self.selected_slot

    def use(self, slot):
        self.c["app_container"] = slot

    def runtime(self, slot, target, phase=None, ready=True, full=True):
        self.use(slot)
        self.runtime_checks.append(full)
        if phase:
            d.require(self.states[slot] == phase, "phase mismatch")
        return {"phase": self.states[slot], "boot_id": self.boots[slot]}

    def control(self, slot, action, boot, seconds=30):
        d.require(boot == self.boots[slot], "stale boot")
        self.event(slot + ":" + action)
        if action == "serve" and self.states[slot] == "standby":
            d.require(all(value == "standby" for name, value in self.states.items() if name != slot), "duplicate owner")
            self.states[slot] = "serving_paused"
        elif action == "resume":
            self.states[slot] = "active"
        elif action == "pause":
            self.states[slot] = "standby"
        return self.runtime(slot, {})

    def stage(self, slot, target):
        d.require(self.states[slot] == "standby", "cannot replace a serving slot")
        self.event(slot + ":stage")
        self.boots[slot] = format(int(self.boots[slot], 16) + 1, "032x")
        return self.runtime(slot, {})

    def settings_hash(self):
        return self.settings

    def service_state(self, name):
        return self.services[name]

    def pause(self, name):
        self.services[name] = False

    def restore(self, name):
        self.event("restore:" + name)
        self.services[name] = True

    def unchanged(self, _, recovering=False):
        self.event("unchanged")

    def gate(self, closed):
        self.event("gate:" + str(closed))
        if closed:
            self.marker.touch()
        else:
            self.marker.unlink(missing_ok=True)

    def switch(self, slot, boot):
        self.selected_slot = slot
        self.event("switch:" + slot)
        self.gate(False)

    def identity(self, slot, boot):
        d.require(slot == self.selected_slot, "wrong ingress")

    def verify(self, target, public=False):
        self.event("verify:" + target["source"])

    def inspect(self, _):
        return {"Image": "new", "State": {"Running": True, "Restarting": False}}


class HandoffTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.attempt = self.root / "activate"
        self.attempt.mkdir()
        d.save(self.root / "prepared.json", {"previous": {"source": "old", "image_id": "old"},
                 "candidate": {"source": "new", "image_id": "new"}, "handoff": {"original_slot": "blue"}})
        d.save(self.attempt / "state.json", {"mode": "activate", "phase": "QUEUED"})
        self.host = FakeHost(self.root)
        self.switch = h.Switch(self.root, self.attempt, self.host)
        self.switch.preflight = lambda: self.host.runtime("blue", {}, "active")
        self.switch.observe = lambda *_: self.host.event("observe")

    def test_switches_traffic_before_resuming_plugin(self):
        self.switch.execute()
        self.assertEqual(self.switch.s["phase"], "ACCEPTED")
        events = self.host.events
        self.assertLess(events.index("green:stage"), events.index("gate:True"))
        self.assertLess(events.index("blue:pause"), events.index("green:serve"))
        self.assertLess(events.index("switch:green"), events.index("green:resume"))
        between = events[events.index("switch:green"):events.index("green:resume")]
        self.assertEqual(between.count("gate:False"), 1)
        self.assertEqual(self.host.states, {"blue": "standby", "green": "active"})
        self.assertFalse(self.host.pending.exists())
        self.assertEqual(self.host.services, {"caller.service": True, "unused.service": False})

    def test_candidate_start_failure_keeps_customer_traffic_open(self):
        self.host.fail = "green:stage"
        with self.assertRaises(RuntimeError):
            self.switch.execute()
        self.assertEqual(self.switch.s["phase"], "RECOVERED")
        self.assertNotIn("gate:True", self.host.events)
        self.assertEqual(self.host.selected(), "blue")

    def test_pause_timeout_cancels_update_without_killing_requests(self):
        self.host.fail = "blue:pause"
        self.switch.execute()
        self.assertEqual(self.switch.s["phase"], "DEFERRED")
        self.assertEqual(self.host.selected(), "blue")
        self.assertNotIn("green:serve", self.host.events)
        self.assertFalse(self.host.marker.exists())

    def test_plugin_failure_reverses_handoff(self):
        self.host.fail = "green:resume"
        with self.assertRaises(RuntimeError):
            self.switch.execute()
        self.assertEqual(self.switch.s["phase"], "RECOVERED")
        self.assertEqual(self.host.states, {"blue": "active", "green": "standby"})
        self.assertEqual(self.host.selected(), "blue")
        self.assertNotIn("restore:unused.service", self.host.events)
        between = self.host.events[self.host.events.index("switch:blue"):self.host.events.index("blue:resume")]
        self.assertEqual(between.count("gate:False"), 1)

    def test_nginx_reload_failure_reverts_pointer_and_owner(self):
        self.host.fail = "switch:green"
        with self.assertRaises(RuntimeError):
            self.switch.execute()
        self.assertEqual(self.switch.s["phase"], "RECOVERED")
        self.assertEqual(self.host.selected(), "blue")

    def test_post_switch_observation_failure_recovers(self):
        self.host.fail = "observe"
        with self.assertRaises(RuntimeError):
            self.switch.execute()
        self.assertEqual(self.switch.s["phase"], "RECOVERED")
        self.assertEqual(self.host.selected(), "blue")

    def test_interrupted_worker_can_resume_recovery(self):
        self.host.fail = "green:resume"
        with patch.object(self.switch, "recover", side_effect=SystemExit):
            with self.assertRaises(SystemExit):
                self.switch.execute()
        recovered = h.Switch(self.root, self.attempt, self.host)
        recovered.recover()
        self.assertEqual(recovered.s["phase"], "RECOVERED")
        recovered.recover()
        self.assertFalse(self.host.pending.exists())

    def test_unresolved_candidate_drain_does_not_create_second_owner(self):
        self.host.states = {"blue": "standby", "green": "active"}
        self.host.selected_slot = "green"
        self.switch.state("RECOVERING", touched=True, services=self.host.services.copy())
        d.atomic(self.host.pending, str(self.attempt).encode())
        self.host.fail = "green:pause"
        with self.assertRaises(RuntimeError):
            self.switch.recover()
        self.assertEqual(self.switch.s["phase"], "NEEDS_ATTENTION")
        self.assertNotIn("blue:serve", self.host.events)
        self.assertTrue(self.host.marker.exists())

    def test_rollback_reverses_slot_names(self):
        path = self.root / "rollback"
        path.mkdir()
        d.save(path / "state.json", {"mode": "rollback", "phase": "QUEUED"})
        switch = h.Switch(self.root, path, self.host)
        self.assertEqual((switch.old, switch.new), ("green", "blue"))

    def assert_recovery_reentry(self, boundary):
        self.host.states = {"blue": "standby", "green": "active"}
        self.host.selected_slot = "green"
        self.host.settings = "new-pricing"
        self.switch.state("RECOVERING", touched=True, services=self.host.services.copy(),
                          old_boot=self.host.boots["blue"], runtime_config="old-pricing")
        d.atomic(self.host.pending, str(self.attempt).encode())
        if boundary == "checkpoint":
            original = self.switch.state
            def interrupt(phase, **fields):
                original(phase, **fields)
                if fields.get("runtime_config") == "new-pricing":
                    raise SystemExit("after checkpoint")
            owner, method = self.switch, "state"
        elif boundary in ("stage", "switch"):
            original = getattr(self.host, boundary)
            def interrupt(slot, value):
                result = original(slot, value)
                if slot == "blue":
                    raise SystemExit("after " + boundary)
                return result
            owner, method = self.host, boundary
        else:
            original = self.host.control
            def interrupt(slot, action, boot, seconds=30):
                result = original(slot, action, boot, seconds)
                if slot == "blue" and action == boundary:
                    raise SystemExit("after " + boundary)
                return result
            owner, method = self.host, "control"
        with patch.object(owner, method, side_effect=interrupt), self.assertRaises(SystemExit):
            self.switch.recover()
        stages = self.host.events.count("blue:stage")
        recovered = h.Switch(self.root, self.attempt, self.host)
        recovered.recover()
        self.assertEqual(recovered.s["phase"], "RECOVERED")
        self.assertEqual(self.host.states, {"blue": "active", "green": "standby"})
        self.assertFalse(self.host.marker.exists())
        self.assertFalse(self.host.pending.exists())
        self.assertEqual(self.host.events.count("blue:stage"), stages + (boundary == "stage"))

    def test_recovery_interrupted_after_refresh_before_checkpoint(self):
        self.assert_recovery_reentry("stage")

    def test_recovery_interrupted_after_refresh_checkpoint(self):
        self.assert_recovery_reentry("checkpoint")

    def test_recovery_interrupted_after_serve(self):
        self.assert_recovery_reentry("serve")

    def test_recovery_interrupted_after_ingress_switch(self):
        self.assert_recovery_reentry("switch")

    def test_recovery_interrupted_after_plugin_resume(self):
        self.assert_recovery_reentry("resume")

    def test_auxiliary_failure_keeps_verified_application_and_retries(self):
        config = self.root / "config.json"
        d.save(config, {})
        record = self.root / "handoff-active.json"
        with patch.object(self.host, "restore", side_effect=RuntimeError("cannot start")):
            self.switch.execute()
            self.assertEqual(self.switch.s["phase"], "ACCEPTED")
            self.assertEqual(self.host.selected(), "green")
            self.assertFalse(self.host.marker.exists())
            self.assertFalse(self.host.pending.exists())
            self.assertEqual(d.read(record)["pending_services"], ["caller.service"])
            self.host.events.clear()
            with patch.object(h, "Host", return_value=self.host):
                h.reconcile({"deployment_config": str(config)})
            self.assertEqual(d.read(record)["pending_services"], ["caller.service"])
            self.assertNotIn("gate:True", self.host.events)
        with patch.object(h, "Host", return_value=self.host):
            h.reconcile({"deployment_config": str(config)})
        self.assertEqual(d.read(record)["pending_services"], [])
        self.assertEqual(self.host.services, {"caller.service": True, "unused.service": False})

    def test_auxiliary_recovery_failure_does_not_close_restored_old_application(self):
        self.host.fail = "green:resume"
        with patch.object(self.host, "restore", side_effect=RuntimeError("cannot start")), self.assertRaises(RuntimeError):
            self.switch.execute()
        self.assertEqual(self.switch.s["phase"], "RECOVERED")
        self.assertEqual(self.host.selected(), "blue")
        self.assertFalse(self.host.marker.exists())
        self.assertEqual(d.read(self.root / "handoff-active.json")["pending_services"], ["caller.service"])

    def test_interrupted_auxiliary_restore_is_durable_without_rollover(self):
        with patch.object(self.host, "restore", side_effect=SystemExit), self.assertRaises(SystemExit):
            self.switch.execute()
        self.assertEqual(d.read(self.attempt / "state.json")["phase"], "ACCEPTED")
        self.assertFalse(self.host.pending.exists())
        self.assertEqual(d.read(self.root / "handoff-active.json")["pending_services"], ["caller.service"])
        config = self.root / "config.json"
        d.save(config, {})
        with patch.object(h, "Host", return_value=self.host):
            h.reconcile({"deployment_config": str(config)})
        self.assertEqual(self.host.selected(), "green")
        self.assertFalse(self.host.marker.exists())
        self.assertEqual(d.read(self.root / "handoff-active.json")["pending_services"], [])

    def test_auxiliary_checkpoint_failure_never_rolls_back_committed_application(self):
        record = self.root / "handoff-active.json"
        save = d.save
        def fail_clear(path, value):
            if path == record and value.get("pending_services") == []:
                raise OSError("cannot persist restored services")
            return save(path, value)
        with patch.object(d, "save", side_effect=fail_clear), self.assertRaises(OSError):
            self.switch.execute()
        self.assertEqual(d.read(self.attempt / "state.json")["phase"], "ACCEPTED")
        self.assertEqual(self.host.selected(), "green")
        self.assertFalse(self.host.marker.exists())
        self.assertEqual(d.read(record)["pending_services"], ["caller.service"])

    def test_recovery_config_change_while_paused_drains_before_refresh(self):
        self.host.states = {"blue": "serving_paused", "green": "standby"}
        self.host.settings = "changed-again"
        self.switch.state("RECOVERING", touched=True, services=self.host.services.copy(),
                          old_boot=self.host.boots["blue"], runtime_config="earlier-checkpoint")
        d.atomic(self.host.pending, str(self.attempt).encode())
        self.switch.recover()
        self.assertLess(self.host.events.index("blue:pause"), self.host.events.index("blue:stage"))
        self.assertEqual(self.switch.s["runtime_config"], "changed-again")
        self.assertEqual(self.switch.s["old_boot"], self.host.boots["blue"])
        self.assertEqual(self.switch.s["phase"], "RECOVERED")

    def test_recovery_boot_mismatch_refreshes_instead_of_reusing_old_checkpoint(self):
        self.host.states = {"blue": "standby", "green": "active"}
        self.host.selected_slot = "green"
        self.switch.state("RECOVERING", touched=True, services=self.host.services.copy(),
                          old_boot="previous-boot", runtime_config=self.host.settings)
        d.atomic(self.host.pending, str(self.attempt).encode())
        self.switch.recover()
        self.assertIn("blue:stage", self.host.events)
        self.assertEqual(self.switch.s["old_boot"], self.host.boots["blue"])

    def test_recovery_drift_during_refresh_does_not_admit_stale_configuration(self):
        self.host.states = {"blue": "standby", "green": "active"}
        self.host.selected_slot = "green"
        self.switch.state("RECOVERING", touched=True, services=self.host.services.copy(), runtime_config="old")
        d.atomic(self.host.pending, str(self.attempt).encode())
        stage = self.host.stage
        def drifting_stage(slot, target):
            result = stage(slot, target)
            self.host.settings = "changed-during-boot"
            return result
        with patch.object(self.host, "stage", side_effect=drifting_stage), self.assertRaisesRegex(RuntimeError, "settings changed"):
            self.switch.recover()
        self.assertNotIn("blue:serve", self.host.events)
        self.assertTrue(self.host.marker.exists())
        h.Switch(self.root, self.attempt, self.host).recover()
        self.assertFalse(self.host.marker.exists())

    def test_new_handoff_cannot_overwrite_pending_service_recovery(self):
        d.save(self.root / "handoff-active.json", {"pending_services": ["caller.service"]})
        with self.assertRaisesRegex(RuntimeError, "auxiliary"):
            h.Switch.preflight(self.switch)
        self.assertEqual(self.host.events, [])

    def test_reconciler_resumes_pending_services_after_application_restart(self):
        self.host.states["blue"] = "standby"
        self.host.services["caller.service"] = False
        config = self.root / "config.json"
        d.save(config, {})
        record = self.root / "handoff-active.json"
        d.save(record, {"slot": "blue", "target": {"source": "old"}, "pending_services": ["caller.service"]})
        with patch.object(h, "Host", return_value=self.host):
            h.reconcile({"deployment_config": str(config)})
        self.assertLess(self.host.events.index("verify:old"), self.host.events.index("restore:caller.service"))
        self.assertEqual(d.read(record)["pending_services"], [])

    def test_auxiliary_recovery_rejects_services_outside_approved_pause_scope(self):
        with self.assertRaisesRegex(RuntimeError, "unapproved"):
            h.restore_pending_services(self.host, {"pending_services": ["unapproved.service"]})
        self.assertEqual(self.host.events, [])

    def test_reconciler_resumes_only_the_persisted_selected_slot(self):
        self.host.states["blue"] = "standby"
        config = self.root / "config.json"
        d.save(config, {})
        d.save(self.root / "handoff-active.json", {"slot": "blue", "target": {"source": "old"}})
        with patch.object(h, "Host", return_value=self.host):
            h.reconcile({"deployment_config": str(config)})
        self.assertEqual(self.host.states, {"blue": "active", "green": "standby"})
        self.assertNotIn("switch:green", self.host.events)

    def test_reconciler_retries_unfinished_active_recovery(self):
        self.host.states["blue"] = "standby"
        config = self.root / "config.json"
        d.save(config, {})
        record = self.root / "handoff-active.json"
        d.save(record, {"slot": "blue", "target": {"source": "old"}})
        self.host.fail = "verify:old"
        with patch.object(h, "Host", return_value=self.host):
            with self.assertRaises(RuntimeError):
                h.reconcile({"deployment_config": str(config)})
            self.assertEqual(self.host.states["blue"], "active")
            self.assertNotIn("boot_id", d.read(record))
            self.host.events.clear()
            h.reconcile({"deployment_config": str(config)})
        self.assertIn("blue:unfreeze", self.host.events)
        self.assertEqual(d.read(record)["boot_id"], self.host.boots["blue"])

    def test_reconciler_does_not_mark_boot_complete_before_unfreeze(self):
        self.host.states["blue"] = "standby"
        config = self.root / "config.json"
        d.save(config, {})
        record = self.root / "handoff-active.json"
        d.save(record, {"slot": "blue", "target": {"source": "old"}})
        self.host.fail = "blue:unfreeze"
        with patch.object(h, "Host", return_value=self.host):
            with self.assertRaises(RuntimeError):
                h.reconcile({"deployment_config": str(config)})
            self.assertNotIn("boot_id", d.read(record))
            h.reconcile({"deployment_config": str(config)})
        self.assertEqual(d.read(record)["boot_id"], self.host.boots["blue"])

    def test_reconciler_healthy_boot_uses_only_lightweight_check(self):
        config = self.root / "config.json"
        d.save(config, {})
        record = self.root / "handoff-active.json"
        d.save(record, {"slot": "blue", "target": {"source": "old"}, "boot_id": self.host.boots["blue"]})
        with patch.object(h, "Host", return_value=self.host):
            h.reconcile({"deployment_config": str(config)})
        self.assertEqual(self.host.runtime_checks, [False])
        self.assertEqual(self.host.events, [])

    def test_reconciler_new_boot_requires_full_verification(self):
        config = self.root / "config.json"
        d.save(config, {})
        record = self.root / "handoff-active.json"
        d.save(record, {"slot": "blue", "target": {"source": "old"}, "boot_id": "previous-boot"})
        with patch.object(h, "Host", return_value=self.host):
            h.reconcile({"deployment_config": str(config)})
        self.assertIn(True, self.host.runtime_checks)
        self.assertIn("verify:old", self.host.events)
        self.assertEqual(d.read(record)["boot_id"], self.host.boots["blue"])

    def test_pending_restart_recovery_rechecks_persistent_data_through_finish(self):
        self.host.states = {"blue": "standby", "green": "standby"}
        self.host.selected_slot = "green"
        checked = []
        def unchanged(_, recovering=False):
            d.require(recovering, "data start time changed")
            checked.append(recovering)
        self.host.unchanged = unchanged
        self.switch.state("RECOVERING", touched=True, services=self.host.services.copy(), runtime_config=self.host.settings)
        d.atomic(self.host.pending, str(self.attempt).encode())
        self.switch.recover()
        self.assertEqual(self.switch.s["phase"], "RECOVERED")
        self.assertEqual(len(checked), 2)
        self.assertEqual(self.host.states, {"blue": "active", "green": "standby"})
        self.assertFalse(self.host.marker.exists())
        self.assertFalse(self.host.pending.exists())

    def test_observation_does_not_repeat_binary_or_asset_checks(self):
        self.host.c["local_url"] = "http://127.0.0.1:18080"
        with patch.object(self.host, "curl", create=True, return_value=b'{"status":"ready"}') as curl, patch.object(h.time, "sleep"):
            h.Switch.observe(self.switch, "blue", {"source": "old"})
        self.assertEqual(self.host.runtime_checks, [False] * 5)
        self.assertEqual(curl.call_count, 5)
        self.assertNotIn("verify:old", self.host.events)

    def test_observation_still_rejects_unready_instance(self):
        self.host.c["local_url"] = "http://127.0.0.1:18080"
        with patch.object(self.host, "curl", create=True, return_value=b'{"status":"not_ready"}'), self.assertRaisesRegex(RuntimeError, "readiness"):
            h.Switch.observe(self.switch, "blue", {"source": "old"})

    def test_reconciler_does_not_override_unowned_maintenance(self):
        config = self.root / "config.json"
        d.save(config, {})
        self.host.marker.touch()
        with patch.object(h, "Host", return_value=self.host), self.assertRaisesRegex(RuntimeError, "unowned"):
            h.reconcile({"deployment_config": str(config)})
        self.assertEqual(self.host.events, [])

    def test_reconciler_leaves_active_deployment_to_lock_owner(self):
        config = self.root / "config.json"
        d.save(config, {})
        with patch.object(h, "Host", return_value=self.host), d.locked(self.host.c):
            h.reconcile({"deployment_config": str(config)})
        self.assertEqual(self.host.events, [])

    def test_staging_configuration_change_refreshes_original_before_recovery(self):
        original_stage = self.host.stage
        def stage(slot, target):
            result = original_stage(slot, target)
            if slot == "green":
                self.host.settings = "updated-by-background"
            return result
        self.host.stage = stage
        with self.assertRaisesRegex(RuntimeError, "settings changed"):
            self.switch.execute()
        self.assertEqual(self.switch.s["phase"], "RECOVERED")
        self.assertIn("blue:stage", self.host.events)
        self.assertEqual(self.host.selected(), "blue")

    def test_nginx_hash_allows_only_selected_upstream_change(self):
        host = object.__new__(h.Host)
        host.upstream = self.root / "upstream.conf"
        host.slots = {"blue": {"app_port": 18080}, "green": {"app_port": 18081}}
        def output(*args):
            return b"events {}\n# configuration file " + str(host.upstream).encode() + b":\n" + host.upstream.read_bytes()
        with patch.object(d, "run", side_effect=output):
            host.upstream.write_bytes(host.line("blue"))
            blue = host.nginx_hash()
            host.upstream.write_bytes(host.line("green"))
            self.assertEqual(host.nginx_hash(), blue)
            host.upstream.write_bytes(host.line("green") + b"server 127.0.0.1:9999;\n")
            with self.assertRaises(RuntimeError):
                host.nginx_hash()


class HostChecks(unittest.TestCase):
    def setUp(self):
        self.host = object.__new__(h.Host)
        self.host.c = {"postgres_container": "pg", "redis_container": "redis"}
        self.info = {key: {"Id": key, "Image": key + "-image", "State": {"Health": {"Status": "healthy"}, "StartedAt": "before"},
                          "Mounts": [{"Type": "volume", "Name": key + "-data", "Source": "/volumes/" + key,
                                      "Destination": "/var/lib/postgresql" if key == "pg" else "/data", "RW": True}]}
                     for key in ("pg", "redis")}
        self.database = {"cluster": "123", "oid": "456", "name": "fixture", "directory": "/var/lib/postgresql/18/docker"}
        self.host.inspect = lambda key: self.info[key]
        self.host.sql = lambda _: json.dumps(self.database)
        self.host.config_hashes = lambda: {}
        self.host.compose_hash = lambda: "compose"
        self.host.env_hashes = lambda: {}
        self.host.nginx_hash = lambda: "nginx"
        self.host.migrations = lambda: {"001": "checksum"}
        self.host.plugins = lambda: []
        self.prepared = {"config_hashes": {}, "compose_sha256": "compose", "handoff": {"env_hashes": {}},
                         "nginx_sha256": "nginx", "data_identity": copy.deepcopy(self.host.data_identity()),
                         "migrations": self.host.migrations(), "plugins": []}

    def test_recovery_allows_only_restart_time_change(self):
        for info in self.info.values():
            info["State"]["StartedAt"] = "after"
        with self.assertRaisesRegex(RuntimeError, "restarted or changed"):
            self.host.unchanged(self.prepared)
        self.host.unchanged(self.prepared, recovering=True)

    def test_recovery_rejects_changed_persistent_identity(self):
        mutations = [lambda: self.info["pg"].update(Id="replacement"),
                     lambda: self.info["redis"].update(Image="replacement"),
                     lambda: self.info["pg"]["Mounts"][0].update(Source="/different"),
                     lambda: self.info["redis"]["Mounts"][0].update(Name="different"),
                     lambda: self.database.update(cluster="different"),
                     lambda: self.database.update(oid="different")]
        for mutate in mutations:
            with self.subTest(mutate=mutate):
                before_info, before_database = copy.deepcopy(self.info), self.database.copy()
                mutate()
                with self.assertRaisesRegex(RuntimeError, "restarted or changed"):
                    self.host.unchanged(self.prepared, recovering=True)
                self.info, self.database = before_info, before_database

    def test_recovery_rejects_missing_persistence_evidence(self):
        del self.prepared["data_identity"]["postgres_container"]["database"]
        with self.assertRaisesRegex(RuntimeError, "persistent data identity missing"):
            self.host.unchanged(self.prepared, recovering=True)

    def test_recovery_rejects_unmounted_redis_data(self):
        self.info["redis"]["Mounts"] = []
        self.prepared["data_identity"] = self.host.data_identity()
        with self.assertRaisesRegex(RuntimeError, "persistent data identity missing"):
            self.host.unchanged(self.prepared, recovering=True)

    def test_recovery_still_checks_migrations_and_plugins(self):
        self.host.migrations = lambda: {"001": "changed"}
        with self.assertRaisesRegex(RuntimeError, "migration history"):
            self.host.unchanged(self.prepared, recovering=True)
        self.host.migrations = lambda: self.prepared["migrations"]
        self.host.plugins = lambda: [{"id": 1, "binary_sha256": "different"}]
        with self.assertRaisesRegex(RuntimeError, "plugin contract"):
            self.host.unchanged(self.prepared, recovering=True)

    def test_lightweight_runtime_still_checks_image_and_source(self):
        self.host.c = {"app_container": "app"}
        self.host.use = lambda _: None
        info = {"Image": "expected-image", "State": {"Running": True, "Restarting": False}}
        self.host.inspect = lambda _: info
        state = {"source": "expected-source", "core_ready": True, "phase": "active"}
        self.host.control = lambda _: state
        target = {"image_id": "expected-image", "source": "expected-source"}
        with patch.object(self.host, "app_facts", side_effect=AssertionError("heavy check")):
            self.host.runtime("blue", target, "active", full=False)
            info["Image"] = "different-image"
            with self.assertRaisesRegex(RuntimeError, "artifact changed"):
                self.host.runtime("blue", target, full=False)
            info["Image"] = target["image_id"]
            state["source"] = "different-source"
            with self.assertRaisesRegex(RuntimeError, "source/dependencies"):
                self.host.runtime("blue", target, full=False)

    def test_switch_uses_private_core_readiness_before_changing_ingress(self):
        with tempfile.TemporaryDirectory() as tmp:
            host = object.__new__(h.Host)
            host.upstream = Path(tmp) / "upstream.conf"
            host.slots = {"blue": {"app_port": 18080}, "green": {"app_port": 18081}}
            d.atomic(host.upstream, host.line("blue"))
            state = {"phase": "serving_paused", "boot_id": "b" * 32, "core_ready": False}
            with patch.object(host, "control", return_value=state), patch.object(host, "gate") as gate, \
                    patch.object(host, "identity") as identity, patch.object(d, "run"):
                with self.assertRaisesRegex(RuntimeError, "core-ready"):
                    host.switch("green", state["boot_id"])
                self.assertEqual(host.selected(), "blue")
                gate.assert_not_called()
                state["core_ready"] = True
                host.switch("green", state["boot_id"])
                self.assertEqual(host.selected(), "green")
                gate.assert_called_once_with(False)
                identity.assert_called_once_with("green", state["boot_id"])


if __name__ == "__main__":
    unittest.main()

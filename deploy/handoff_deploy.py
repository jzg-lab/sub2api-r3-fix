"""Two fixed local slots, one shared database, one plugin/task owner."""
import hashlib
import json
from pathlib import Path
import re
import subprocess
import time
from urllib.parse import urlsplit

import fast_deploy as d


def restore_pending_services(host, saved):
    pending = saved.get("pending_services", [])
    d.require(set(pending) <= set(host.c["pause_services"]), "unapproved auxiliary recovery service")
    remaining = []
    for name in pending:
        try:
            host.restore(name)
        except (RuntimeError, OSError, subprocess.SubprocessError) as error:
            remaining.append(name)
            print(json.dumps({"auxiliary_restore_pending": name, "error_type": type(error).__name__}), flush=True)
    if remaining != pending:
        d.save(Path(host.c["deploy_dir"]) / "handoff-active.json", {**saved, "pending_services": remaining})


def retire_previous(host, saved):
    peer = saved.get("retiring")
    if not peer:
        return
    try:
        d.require(host.selected() == saved["slot"] and peer["slot"] != saved["slot"], "retirement slot changed")
        current = host.runtime(saved["slot"], saved["target"], "active", full=False)
        d.require(current["boot_id"] == saved["boot_id"], "selected boot changed during retirement")
        state = host.runtime(peer["slot"], peer["target"], ready=False, full=False)
        d.require(state["boot_id"] == peer["boot_id"] and state["phase"] in ("released", "standby"),
                  "retiring instance changed")
        # Reloaded Nginx workers may still own HTTP/2 or WebSocket connections.
        # A zero application request count alone does not make replacement safe.
        if set(peer["workers"]) & set(host.nginx_workers()):
            return
        if state["requests"] or state["background"] or host.connections(peer["slot"]):
            return
        host.control(peer["slot"], "pause", state["boot_id"], host.c["drain_seconds"])
        host.control(saved["slot"], "unfreeze", current["boot_id"])
        d.save(Path(host.c["deploy_dir"]) / "handoff-active.json", {**saved, "retiring": None})
    except (RuntimeError, OSError, subprocess.SubprocessError) as error:
        print(json.dumps({"retirement_pending": peer["slot"], "error_type": type(error).__name__}), flush=True)
    finally:
        host.use(saved["slot"])


class Host(d.Host):
    def __init__(self, config):
        self.site = config
        self.handoff = config["handoff"]
        self.slots = self.handoff["slots"]
        d.require(set(self.slots) == {"blue", "green"}, "exactly two fixed slots required")
        self.upstream = Path(self.handoff["upstream_file"])
        d.require(self.upstream.is_absolute() and not self.upstream.is_symlink(), "invalid upstream include")
        self.pricing = Path(self.handoff["pricing_file"])
        d.require(self.pricing.is_absolute() and self.pricing.is_file() and not self.pricing.is_symlink(), "existing pricing file required")
        stable = urlsplit(self.handoff["stable_url"])
        d.require(stable.scheme == "http" and stable.hostname == "127.0.0.1" and stable.port == 8080
                  and not stable.username and stable.path in ("", "/") and not stable.query and not stable.fragment,
                  "existing local callers require stable loopback port 8080")
        for slot in self.slots.values():
            d.validate_config({**config, **slot})
            d.require(Path(slot["control_socket"]).is_absolute(), "absolute private socket required")
        for field in ("app_container", "app_port", "control_socket", "env_file", "compose_file"):
            d.require(len({s[field] for s in self.slots.values()}) == 2, "slot must be independent: " + field)
        super().__init__({**config, **self.slots[self.selected()]})

    def line(self, slot):
        return ("server 127.0.0.1:%d;\n" % self.slots[slot]["app_port"]).encode()

    def selected(self):
        content = self.upstream.read_bytes()
        found = [slot for slot in self.slots if content == self.line(slot)]
        d.require(len(found) == 1, "unknown upstream; explicit installation required")
        return found[0]

    def use(self, slot):
        self.c = {**self.site, **self.slots[slot]}

    def control(self, slot, action=None, boot=None, seconds=30):
        path = Path(self.slots[slot]["control_socket"])
        d.require(path.is_socket() and not path.is_symlink(), "runtime socket unavailable")
        args = ["curl", "-q", "--noproxy", "*", "-sS", "--max-time", str(seconds + 5),
                "--unix-socket", str(path), "--write-out", "\n%{http_code}"]
        if action:
            d.require(isinstance(boot, str) and re.fullmatch(r"[0-9a-f]{32}", boot), "boot-bound command required")
            args += ["-H", "Content-Type: application/json", "--data-binary",
                     json.dumps({"action": action, "boot_id": boot, "timeout_seconds": seconds})]
        body, code = d.run(*args, "http://localhost/" + ("transition" if action else "status"), timeout=seconds + 7).rsplit(b"\n", 1)
        d.require(code == b"200", "runtime transition failed: " + (action or "status"))
        state = json.loads(body)
        d.require(re.fullmatch(r"[0-9a-f]{32}", state["boot_id"]) and state["phase"] in
                  ("standby", "draining", "serving_paused", "active", "releasing", "released"), "invalid runtime status")
        return state

    def runtime(self, slot, target, phase=None, ready=True, full=True):
        self.use(slot)
        if full:
            facts = self.app_facts()
            d.require(all(facts[k] == target[k] for k in ("image_id", "binary_sha256")), "slot artifact changed")
        else:
            info = self.inspect(self.c["app_container"])
            d.require(info["State"]["Running"] and not info["State"]["Restarting"] and
                      info["Image"] == target["image_id"], "slot artifact changed or stopped")
        state = self.control(slot)
        d.require(state["source"] == target["source"] and (state["core_ready"] or not ready), "slot source/dependencies differ")
        if phase:
            d.require(state["phase"] == phase, "unexpected slot phase")
        return state

    def identity(self, slot, boot):
        origins = [self.handoff["stable_url"], *self.c["public_urls"]]
        for origin in origins:
            headers = d.run("curl", "-q", "--noproxy", "*", "-fsS", "--max-time", "5",
                            "-H", "Connection: close", "-D", "-", "-o", "/dev/null", origin.rstrip("/") + "/health")
            ids = re.findall(rb"(?im)^x-sub2api-instance:\s*([^\r\n]+)", headers)
            d.require(ids == [boot.encode()], "ingress does not target expected instance: " + origin)
        d.require(self.selected() == slot, "upstream pointer changed")

    def nginx_hash(self):
        raw = d.run("nginx", "-T")
        block = b"# configuration file " + str(self.upstream).encode() + b":\n" + self.line(self.selected())
        d.require(raw.count(block) == 1, "upstream include absent or ambiguous")
        return hashlib.sha256(raw.replace(block, b"# fixed handoff upstream\n")).hexdigest()

    def nginx_workers(self):
        result = []
        for line in d.run("ps", "-C", "nginx", "-o", "pid=,args=").decode().splitlines():
            pid, command = line.strip().split(None, 1)
            if command.startswith("nginx: worker process"):
                try:
                    stat = Path("/proc", pid, "stat").read_text().rsplit(") ", 1)[1].split()
                    result.append(pid + ":" + stat[19])
                except FileNotFoundError:
                    pass  # A worker can finish between ps and /proc.
        return result

    def connections(self, slot):
        self.use(slot)
        raw = d.run("docker", "exec", self.c["app_container"], "cat", "/proc/net/tcp", "/proc/net/tcp6").decode()
        service = json.loads(self.compose("config", "--format", "json"))["services"][self.c["app_service"]]
        port = ":%04X" % int(service["ports"][0]["target"])
        return sum(len(p) > 3 and p[1].endswith(port) and p[3] in ("01", "08")
                   for p in map(str.split, raw.splitlines()))

    def verify_live_ingress(self):
        raw = d.run("nginx", "-T").decode()
        values = re.findall(r"(?m)^[^#\r\n]*?\bworker_shutdown_timeout\s+([^;]+);", raw)
        d.require(all(re.fullmatch(r"0(?:ms|s|m|h|d|w)?", value.strip()) for value in values),
                  "finite nginx worker shutdown timeout can interrupt old streams")
        d.require(self.nginx_workers(), "nginx workers unavailable")

    def switch(self, slot, boot):
        state = self.control(slot)
        d.require(state["boot_id"] == boot and state["phase"] in ("serving_paused", "active") and
                  state["core_ready"], "switch target is not core-ready")
        d.atomic(self.upstream, self.line(slot), 0o644)
        d.run("nginx", "-t")
        d.run("systemctl", "reload", "nginx")
        self.gate(False)
        deadline = time.monotonic() + 10
        while True:
            try:
                self.identity(slot, boot)
                return
            except RuntimeError:
                d.require(time.monotonic() < deadline, "ingress switch not observed")
                time.sleep(0.2)

    def config_hashes(self):
        paths = {self.site["admin_header_file"], *self.site["snapshot_files"],
                 *(s["compose_file"] for s in self.slots.values())}
        d.require(str(self.upstream) not in paths, "upstream is checked separately")
        return {p: d.digest(p) for p in sorted(paths)}

    def compose_hash(self):
        previous, result = self.c, {}
        try:
            for slot in self.slots:
                self.use(slot)
                result[slot] = super().compose_hash()
        finally:
            self.c = previous
        return hashlib.sha256(json.dumps(result, sort_keys=True).encode()).hexdigest()

    def env_hashes(self):
        return {slot: hashlib.sha256(d.image_env(Path(c["env_file"]).read_bytes(), "normalized")).hexdigest()
                for slot, c in self.slots.items()}

    def settings_hash(self):
        return {"settings": self.sql("SELECT md5(coalesce(json_agg(s ORDER BY key)::text, '[]')) FROM settings s"),
                "pricing": d.digest(self.pricing)}

    def data_identity(self):
        result = super().data_identity()
        for key, identity in result.items():
            mounts = self.inspect(self.c[key])["Mounts"]
            identity["mounts"] = sorted(mounts, key=lambda mount: mount["Destination"])
        result["postgres_container"]["database"] = json.loads(self.sql(
            "SELECT json_build_object('cluster', system_identifier::text, 'oid', d.oid::text, "
            "'name', d.datname, 'directory', current_setting('data_directory')) "
            "FROM pg_control_system(), pg_database d WHERE d.datname = current_database()"))
        return result

    def verify_ingress(self):
        slot = self.selected()
        state = self.control(slot)
        d.require(state["phase"] == "active", "selected instance not active")
        self.identity(slot, state["boot_id"])
        for name in self.slots:
            self.use(name)
            compose = json.loads(self.compose("config", "--format", "json"))
            service = compose["services"][self.c["app_service"]]
            env = service.get("environment", {})
            d.require(env.get("DEPLOYMENT_SOCKET") and env.get("DEPLOYMENT_OWNER_LOCK"), "slot lacks handoff runtime")
            ports = service.get("ports", [])
            d.require(len(ports) == 1 and ports[0].get("host_ip") == "127.0.0.1" and
                      int(ports[0]["published"]) == self.c["app_port"], "slot port is not private/fixed")
            mounts = {m["target"]: m for m in service.get("volumes", [])}
            data = mounts.get("/app/data", {})
            d.require(data.get("type") == "bind" and not data.get("read_only"), "shared existing data bind required")
            owner = Path(env["DEPLOYMENT_OWNER_LOCK"])
            socket = Path(env["DEPLOYMENT_SOCKET"])
            d.require(owner.is_relative_to("/app/data") and socket.is_relative_to("/app/data"), "runtime must use shared data bind")
            d.require(Path(data["source"]) / socket.relative_to("/app/data") == Path(self.c["control_socket"]), "socket mount differs")
            d.require(str(env.get("AUTO_SETUP", "false")).lower() == "false", "managed slots must not initialize data")
            contract = {"mounts": mounts, "owner": str(owner),
                        "environment": {k: v for k, v in env.items() if k != "DEPLOYMENT_SOCKET"}}
            if name == "blue":
                blue_contract = contract
            else:
                # Compare below outside iteration order.
                green_contract = contract
        d.require(blue_contract == green_contract, "slots do not share the same data/configuration")
        self.use(slot)

    def unchanged(self, prepared, recovering=False):
        d.require(self.config_hashes() == prepared["config_hashes"], "configuration changed")
        d.require(self.compose_hash() == prepared["compose_sha256"], "Compose configuration changed")
        d.require(self.env_hashes() == prepared["handoff"]["env_hashes"], "non-image slot environment changed")
        d.require(self.nginx_hash() == prepared["nginx_sha256"], "nginx configuration changed")
        current, previous = self.data_identity(), prepared["data_identity"]
        if recovering:
            # Only restart timestamps may differ; old receipts without persistent
            # identity are intentionally insufficient for unattended recovery.
            for key in ("postgres_container", "redis_container"):
                directory = previous[key].get("database", {}).get("directory") if key == "postgres_container" else "/data"
                d.require(directory and any(m.get("Type") in ("volume", "bind") and m.get("RW") and
                          Path(directory).is_relative_to(m["Destination"]) for m in previous[key].get("mounts", [])),
                          "persistent data identity missing; manual recovery required")
            current = {k: {f: v for f, v in item.items() if f != "started"} for k, item in current.items()}
            previous = {k: {f: v for f, v in item.items() if f != "started"} for k, item in previous.items()}
        d.require(current == previous, "database/Redis restarted or changed")
        d.require(self.migrations() == prepared["migrations"], "migration history changed; rollback prohibited")
        d.require(d.plugin_contract(self.plugins()) == d.plugin_contract(prepared["plugins"]), "plugin contract changed")

    def stage(self, slot, target):
        self.use(slot)
        ids = self.compose("ps", "--all", "--quiet", self.c["app_service"]).strip()
        if ids:
            info = self.inspect(self.c["app_container"])
            if info["State"]["Running"]:
                d.require(self.control(slot)["phase"] == "standby", "cannot replace a serving slot")
        d.require(self.inspect(target["image"], image=True)["Id"] == target["image_id"], "image tag changed")
        env = Path(self.c["env_file"])
        d.atomic(env, d.image_env(env.read_bytes(), target["image"]))
        # Restart the standby to discard caches left from an earlier release.
        self.compose("up", "-d", "--no-deps", "--force-recreate", "--no-build", "--pull", "never", self.c["app_service"])
        deadline = time.monotonic() + self.c["ready_seconds"]
        while True:
            try:
                state = self.runtime(slot, target, "standby")
                self.check_applied_compose()
                return state
            except (RuntimeError, OSError):
                d.require(time.monotonic() < deadline, "standby startup failed")
                time.sleep(0.5)


class Switch(d.Switch):
    def __init__(self, release, attempt, host=None):
        super().__init__(release, attempt, host or Host(d.read(release / "config.json")))
        original = self.p["handoff"]["original_slot"]
        self.old = original if self.s["mode"] == "activate" else self.other(original)
        self.new = self.other(self.old)
        self.live = self.p["handoff"].get("protocol", 1) == 2

    @staticmethod
    def other(slot):
        return "green" if slot == "blue" else "blue"

    def preflight(self):
        active = Path(self.h.c["deploy_dir"]) / "handoff-active.json"
        d.require(not active.exists() or not d.read(active).get("pending_services"), "auxiliary services still require recovery")
        d.require(not active.exists() or not d.read(active).get("retiring"), "previous instance still serving old connections")
        # The legacy preflight's single .env binding is not applicable to two slots.
        for name, checksum in self.p["sealed_files"].items():
            d.require(d.digest(self.release / name) == checksum, "prepared file changed: " + name)
        d.require(0 <= time.time() - self.p["backup_at"] <= self.h.c["backup_max_age_seconds"], "backup expired")
        d.require(d.digest(self.release / "postgres.dump") == self.p["backup_sha256"], "backup changed")
        d.evidence(self.release, d.read(self.release / "artifact.json"), self.h)
        self.h.unchanged(self.p)
        for target in (self.original, self.target):
            d.require(self.h.inspect(target["image"], image=True)["Id"] == target["image_id"], "image tag changed")
        d.require(not self.h.pending.exists() and not self.h.marker.exists(), "recovery pending")
        d.require(self.h.selected() == self.old, "selected slot changed")
        self.h.verify_ingress()
        state = self.h.runtime(self.old, self.original, "active")
        if self.live:
            d.require(state.get("handoff_protocol") == 2, "live handoff runtime unavailable")
            self.h.verify_live_ingress()
        self.h.verify(self.original, public=True)
        self.h.check_applied_compose()
        return state

    def finish(self, phase, slot, target):
        state = self.h.runtime(slot, target, "active")
        self.h.unchanged(self.p, recovering=phase == "RECOVERED")
        self.h.verify(target)
        self.h.gate(False)
        self.h.identity(slot, state["boot_id"])
        self.h.verify(target, public=True)
        saved = {"slot": slot, "target": target, "boot_id": state["boot_id"],
                 "pending_services": [name for name, active in self.s.get("services", {}).items() if active]}
        if self.live and self.s.get("new_boot") and "retiring_workers" in self.s:
            peer = self.other(slot)
            peer_target = self.original if peer == self.old else self.target
            self.h.use(peer)
            info = self.h.inspect(self.h.c["app_container"])
            d.require(info["Image"] == peer_target["image_id"] and not info["State"]["Restarting"], "retiring artifact unresolved")
            if info["State"]["Running"]:
                peer_state = self.h.runtime(peer, peer_target, ready=False, full=False)
                d.require(peer_state["phase"] in ("released", "standby"), "peer still owns background work")
                if peer_state["phase"] == "released":
                    saved["retiring"] = {"slot": peer, "target": peer_target, "boot_id": peer_state["boot_id"],
                                         "workers": self.s["retiring_workers"]}
        if not saved.get("retiring"):
            self.h.control(slot, "unfreeze", state["boot_id"])
        self.h.use(slot)
        d.save(Path(self.h.c["deploy_dir"]) / "handoff-active.json", saved)
        self.state(phase)
        self.h.pending.unlink(missing_ok=True)
        # The verified application is committed before restarting auxiliary callers.
        restore_pending_services(self.h, saved)
        retire_previous(self.h, d.read(Path(self.h.c["deploy_dir"]) / "handoff-active.json"))

    def execute(self):
        try:
            old = self.preflight()
        except Exception:
            self.state("PREFLIGHT_FAILED")
            raise
        services = {name: self.h.service_state(name) for name in self.h.c["pause_services"]}
        self.state("STAGING", touched=True, services=services, old_boot=old["boot_id"])
        d.atomic(self.h.pending, str(self.attempt).encode())
        try:
            self.h.control(self.old, "freeze", old["boot_id"])
            settings = self.h.settings_hash()
            self.state("STAGING", runtime_config=settings)
            candidate = self.h.stage(self.new, self.target)
            if self.live:
                d.require(candidate.get("handoff_protocol") == 2, "candidate lacks live handoff protocol")
            self.state("PAUSING", new_boot=candidate["boot_id"])
            for name, active in services.items():
                if active:
                    self.h.pause(name)
                d.require(not self.h.service_state(name), "direct caller still active")
            if not self.live:
                self.h.gate(True)
            self.state("RELEASING" if self.live else "DRAINING")
            try:
                self.h.control(self.old, "release" if self.live else "pause", old["boot_id"],
                               self.h.c["ready_seconds"] if self.live else self.h.c["drain_seconds"])
            except RuntimeError:
                if self.h.runtime(self.old, self.original)["phase"] == "active":
                    self.finish("DEFERRED", self.old, self.original)
                    return
                raise
            self.h.unchanged(self.p)
            d.require(settings == self.h.settings_hash(), "settings changed during standby boot; retry preparation")
            self.state("SWITCHING", retiring_workers=self.h.nginx_workers() if self.live else [])
            self.h.control(self.new, "serve", candidate["boot_id"])
            self.h.switch(self.new, candidate["boot_id"])
            self.state("RESUMING", public_opened=True)
            self.h.control(self.new, "resume", candidate["boot_id"], self.h.c["ready_seconds"])
            self.state("PUBLIC_VERIFY")
            self.observe(self.new, self.target)
            self.finish("ACCEPTED" if self.s["mode"] == "activate" else "ROLLED_BACK", self.new, self.target)
        except Exception as error:
            if self.s["phase"] not in d.TERMINAL:
                self.state("RECOVERING", error_type=type(error).__name__)
                self.recover()
            raise

    def observe(self, slot, target):
        for _ in range(5):
            state = self.h.runtime(slot, target, "active", full=False)
            self.h.identity(slot, state["boot_id"])
            d.require(json.loads(self.h.curl(self.h.c["local_url"], "/readyz"))["status"] == "ready", "readiness failed")
            time.sleep(2)

    def recover(self):
        if self.s["phase"] in d.TERMINAL:
            if self.h.pending.exists() and self.h.pending.read_text() == str(self.attempt) and not self.h.marker.exists():
                self.h.pending.unlink()
            return
        if not self.s.get("touched"):
            self.state("PREFLIGHT_FAILED")
            return
        if self.s["phase"] == "STAGING" and not self.h.pending.exists():
            self.state("PREFLIGHT_FAILED")
            return
        try:
            d.require(self.h.pending.read_text() == str(self.attempt), "recovery ownership unresolved")
            self.h.unchanged(self.p, recovering=True)
            old = self.h.runtime(self.old, self.original)
            # A standby build failure must not close healthy customer traffic.
            if old["phase"] == "active" and self.h.selected() == self.old:
                self.finish("RECOVERED", self.old, self.original)
                return
            if self.live:
                d.require(self.s.get("runtime_config") == self.h.settings_hash() and
                          self.s.get("old_boot") == old["boot_id"],
                          "live rollback configuration changed; manual recovery required")
            if not self.live:
                self.h.gate(True)
            for name, active in self.s["services"].items():
                if active:
                    self.h.pause(name)
            self.h.use(self.new)
            info = self.h.inspect(self.h.c["app_container"])
            d.require(info["Image"] == self.target["image_id"] and not info["State"]["Restarting"], "candidate identity unresolved")
            if info["State"]["Running"]:
                candidate = self.h.runtime(self.new, self.target, ready=False)
                self.h.control(self.new, "release" if self.live else "pause", candidate["boot_id"],
                               self.h.c["ready_seconds"] if self.live else self.h.c["drain_seconds"])
            # Background sync may have updated cached settings/prices after cutover.
            settings = self.h.settings_hash()
            if self.s.get("runtime_config") != settings or self.s.get("old_boot", old["boot_id"]) != old["boot_id"]:
                # Never restart a live old process with requests still attached.
                # Config drift requires a separate recovery decision in live mode.
                d.require(not self.live, "live rollback configuration changed; manual recovery required")
                if old["phase"] != "standby":
                    self.h.control(self.old, "pause", old["boot_id"], self.h.c["drain_seconds"])
                old = self.h.stage(self.old, self.original)
                d.require(settings == self.h.settings_hash(), "settings changed during recovery boot")
                self.state("RECOVERING", runtime_config=settings, old_boot=old["boot_id"])
            self.h.control(self.old, "serve", old["boot_id"])
            if self.live:
                self.state("RECOVERING", retiring_workers=self.h.nginx_workers())
            self.h.switch(self.old, old["boot_id"])
            self.h.control(self.old, "resume", old["boot_id"], self.h.c["ready_seconds"])
            self.finish("RECOVERED", self.old, self.original)
        except Exception as error:
            if self.s["phase"] not in d.TERMINAL:
                if not self.live:
                    d.atomic(self.h.marker, b"recovery required\n", 0o644)
                self.state("NEEDS_ATTENTION", recovery_error_type=type(error).__name__)
            raise


def reconcile(config):
    """Installed timer repairs interrupted jobs and restarts of the selected slot."""
    host = Host(d.read(config["deployment_config"]))
    try:
        with d.locked(host.c):
            if host.pending.exists():
                attempt = Path(host.pending.read_text())
                root = Path(config["state_dir"]).resolve()
                d.require(attempt.is_absolute() and root in attempt.resolve().parents and
                          attempt.name in ("activate", "rollback") and attempt.parent.name == "release", "unknown recovery path")
                release = attempt.parent
                prepared = d.read(release / "prepared.json")
                d.require(d.read(release / "config.json") == host.site, "pending recovery belongs to a different site")
                for name, checksum in prepared["sealed_files"].items():
                    d.require(release.resolve() in (release / name).resolve().parents and
                              d.digest(release / name) == checksum, "recovery material changed")
            else:
                d.require(not host.marker.exists(), "unowned maintenance; manual review required")
                saved = d.read(Path(host.c["deploy_dir"]) / "handoff-active.json")
                slot, target = saved["slot"], saved["target"]
                d.require(host.selected() == slot, "selected slot differs from accepted record")
                state = host.runtime(slot, target, full=False)
                if state["phase"] == "active" and state["boot_id"] == saved.get("boot_id"):
                    restore_pending_services(host, saved)
                    retire_previous(host, d.read(Path(host.c["deploy_dir"]) / "handoff-active.json"))
                    return
                d.require(state["phase"] in ("standby", "serving_paused", "active"), "runtime operation unresolved")
                # A boot is complete only after verification and configuration
                # unfreeze. Retry this tail even if resume already made it active.
                state = host.runtime(slot, target)
                # The shared lock also excludes an unexpected live peer.
                host.control(slot, "serve", state["boot_id"])
                host.identity(slot, state["boot_id"])
                host.control(slot, "resume", state["boot_id"], host.c["ready_seconds"])
                host.verify(target, public=True)
                if not saved.get("retiring"):
                    host.control(slot, "unfreeze", state["boot_id"])
                saved = {**saved, "boot_id": state["boot_id"]}
                d.save(Path(host.c["deploy_dir"]) / "handoff-active.json", saved)
                restore_pending_services(host, saved)
                retire_previous(host, d.read(Path(host.c["deploy_dir"]) / "handoff-active.json"))
                return
    except BlockingIOError:
        return
    # The sealed worker acquires the same lock itself, after this probe releases it.
    d.run("/usr/bin/python3", str(release / "tools/fast_deploy.py"), "_recover", "--release", str(release),
          "--attempt", attempt.name, timeout=300)

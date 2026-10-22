"""Real candidate round-trip in a disposable, internal-only Docker Compose project."""
import hashlib
import json
import os
from pathlib import Path
import secrets
import time
from urllib.parse import urlsplit

import fast_deploy as d


def caller_inventory(host):
    units = d.run("systemctl", "list-unit-files", "--type=service", "--type=timer", "--no-legend", "--no-pager")
    details = [d.run("systemctl", "show", name, "--property=FragmentPath,DropInPaths,ExecStart,Restart,Triggers,TriggeredBy")
               for name in host.c["pause_services"]]
    return hashlib.sha256(units + b"\n".join(details)).hexdigest()


def check_runtime_uid(container):
    status = d.run("docker", "exec", container, "cat", "/proc/1/status").decode()
    fields = dict(line.split(":", 1) for line in status.splitlines() if ":" in line)
    # docker exec inherits the image user, not the entrypoint's dropped identity.
    d.require(fields.get("Uid", "").split() == ["1000"] * 4, "runtime UID changed")


class LabHost(d.Host):
    # Docker's internal network stays closed. Probes execute inside the fixture,
    # so no host port, proxy, production mount or external route is needed.
    def curl(self, origin, path, admin=False, expected=200, timeout=5):
        d.require(origin == self.c["local_url"], "fixture probes must remain local")
        args = ["curl", "-q", "--noproxy", "*", "-sS", "--max-time", str(timeout), "-H", "Connection: close"]
        if admin:
            args += ["-H", "@/app/data/test-admin-header.txt"]
        raw = d.run("docker", "exec", self.c["app_container"], *args, "-w", "\n%{http_code}", origin + path, timeout=timeout + 2)
        body, code = raw.rsplit(b"\n", 1)
        d.require(int(code) == expected, "fixture HTTP check failed: " + path)
        return body


class Fixture:
    def __init__(self, job, host, artifact, inputs):
        self.mode = host.c.get("mode", "single")
        self.root = job / "lab"
        self.root.mkdir(mode=0o700)
        self.data = self.root / "data"
        self.data.mkdir(mode=0o700)
        os.chown(self.data, 1000, 1000)
        self.artifact, self.inputs = artifact, inputs
        self.started = False
        name = "sub2api-check-" + job.name
        self.names = {key: name + "-" + key for key in ("app", "postgres", "redis")}
        password, self.password = secrets.token_hex(24), secrets.token_hex(24)
        self.environment = {
            "AUTO_SETUP": "true", "DATABASE_HOST": "postgres", "DATABASE_PORT": "5432",
            "DATABASE_USER": "sub2api", "DATABASE_PASSWORD": password, "DATABASE_DBNAME": "sub2api",
            "DATABASE_SSLMODE": "disable", "REDIS_HOST": "redis", "REDIS_PORT": "6379",
            "SERVER_HOST": "0.0.0.0", "SERVER_PORT": "17880", "JWT_SECRET": secrets.token_hex(32),
            "ADMIN_EMAIL": "deployment@example.test", "ADMIN_PASSWORD": self.password,
            "TOTP_ENCRYPTION_KEY": secrets.token_hex(32), "PLUGINS_DATA_DIR": "data/plugins",
        }
        self.compose = {"name": name, "services": {
            "sub2api": {"image": "${SUB2API_IMAGE}", "container_name": self.names["app"],
                        "environment": self.environment, "volumes": [str(self.data) + ":/app/data"],
                        "cpus": 2, "mem_limit": "4g"},
            "postgres": {"image": host.inspect(host.c["postgres_container"])["Image"],
                         "container_name": self.names["postgres"], "mem_limit": "1g", "cpus": 1,
                         "environment": {"POSTGRES_USER": "sub2api", "POSTGRES_DB": "sub2api", "POSTGRES_PASSWORD": password},
                         "volumes": ["pg:/var/lib/postgresql"],
                         "healthcheck": {"test": ["CMD", "pg_isready", "-U", "sub2api", "-d", "sub2api"], "interval": "1s", "retries": 60}},
            "redis": {"image": host.inspect(host.c["redis_container"])["Image"],
                      "container_name": self.names["redis"], "mem_limit": "256m", "cpus": 1,
                      "healthcheck": {"test": ["CMD", "redis-cli", "ping"], "interval": "1s", "retries": 60}},
        }, "volumes": {"pg": {}}, "networks": {"default": {"internal": True}}}
        d.save(self.root / "compose.json", self.compose)
        d.atomic(self.root / ".env", ("SUB2API_IMAGE=" + artifact["base_image_id"] + "\n").encode())
        config = {**host.c, "deploy_dir": str(self.root), "compose_file": str(self.root / "compose.json"),
                  "env_file": str(self.root / ".env"), "app_service": "sub2api", "app_container": self.names["app"],
                  "postgres_container": self.names["postgres"], "redis_container": self.names["redis"],
                  "database_user": "sub2api", "database_name": "sub2api", "local_url": "http://127.0.0.1:17880",
                  "app_port": 17880, "public_urls": ["https://fixture.invalid"], "guard_checks": [], "pause_services": [],
                  "snapshot_files": [], "admin_header_file": str(self.data / "test-admin-header.txt"),
                  "maintenance_file": str(self.root / "maintenance"), "ready_seconds": 90}
        self.h = LabHost(config)

    def file(self, name, content):
        path = self.data / name
        d.atomic(path, content)
        os.chown(path, 1000, 1000)
        return "/app/data/" + name

    def api(self, path, method="GET", body=None, jwt=False, upload=False, raw=False):
        args = ["curl", "-q", "--noproxy", "*", "-sS", "--max-time", "30", "-X", method]
        header = "test-jwt-header.txt" if jwt else "test-admin-header.txt"
        if (self.data / header).exists():
            args += ["-H", "@/app/data/" + header]
        if body is not None:
            args += ["-H", "Content-Type: application/json", "--data-binary", "@" + self.file("test-request.json", json.dumps(body).encode())]
        if upload:
            args += ["-F", "plugin=@/app/data/test-plugin.s2plugin"]
        response = d.run("docker", "exec", self.names["app"], *args, "-w", "\n%{http_code}",
                    self.h.c["local_url"] + "/api/v1" + path, timeout=35)
        body, code = response.rsplit(b"\n", 1)
        d.require(int(code) in (200, 201), "fixture API failed: " + path)
        result = json.loads(body)
        if raw:
            d.require(isinstance(result, dict), "fixture config response must be an object")
            return result
        d.require(result.get("code") == 0, "fixture API rejected request: " + path)
        return result["data"]

    def wait(self, ready=True):
        deadline = time.monotonic() + 90
        while time.monotonic() < deadline:
            try:
                self.h.curl(self.h.c["local_url"], "/readyz" if ready else "/health")
                return
            except Exception:
                time.sleep(1)
        raise RuntimeError("fixture startup deadline exceeded")

    def setup(self):
        for name in self.names.values():
            d.require(not d.run("docker", "ps", "-aq", "--filter", "name=^/" + name + "$").strip(), "fixture name already exists")
        self.started = True
        self.h.compose("up", "-d", "--pull", "never", "postgres", "redis")
        deadline = time.monotonic() + 60
        while True:
            try:
                self.h.data_identity()
                break
            except Exception:
                d.require(time.monotonic() < deadline, "fixture database startup failed")
                time.sleep(1)
        self.h.compose("up", "-d", "--no-deps", "--pull", "never", "sub2api")
        self.wait(False)
        self.h.stop(self.root / "setup-stop.log")
        # The trust keys are the approved public keys, never a regenerated signer.
        config = {"database": {"host": "postgres", "port": 5432, "user": "sub2api", "password": self.environment["DATABASE_PASSWORD"],
                               "dbname": "sub2api", "sslmode": "disable"}, "redis": {"host": "redis", "port": 6379},
                  "server": {"host": "0.0.0.0", "port": 17880, "mode": "release"},
                  "jwt": {"secret": self.environment["JWT_SECRET"], "expire_hour": 24},
                  "plugins": d.read(self.inputs["trust"]["path"])["plugins"]}
        config["plugins"].update(max_upload_bytes=128 * 1024 * 1024, max_uncompressed_bytes=256 * 1024 * 1024)
        self.file("config.yaml", json.dumps(config).encode())
        self.h.compose("up", "-d", "--no-deps", "--pull", "never", "sub2api")
        self.wait(False)
        token = self.api("/auth/login", "POST", {"email": "deployment@example.test", "password": self.password})["access_token"]
        self.file("test-jwt-header.txt", ("Authorization: Bearer " + token + "\n").encode())
        compliance = self.api("/admin/compliance", jwt=True)
        if compliance["required"]:
            self.api("/admin/compliance/accept", "POST", {"phrase": compliance["ack_phrase_en"], "language": "en"}, jwt=True)
        key = self.api("/admin/settings/admin-api-key/regenerate", "POST", jwt=True)["key"]
        self.file("test-admin-header.txt", ("x-api-key: " + key + "\n").encode())
        self.file("test-plugin.s2plugin", Path(self.inputs["package"]["path"]).read_bytes())
        plugin = self.api("/admin/plugins/upload", "POST", upload=True)
        d.require(plugin["signature_status"] == "trusted" and plugin["compatibility"]["compatible"], "fixture plugin incompatible")
        self.plugin_id = plugin["id"]
        self.api("/admin/plugins/%s/config" % self.plugin_id, "PUT", d.read(self.inputs["plugin_config"]["path"]), raw=True)
        self.api("/admin/plugins/%s/enable" % self.plugin_id, "POST", {"accept_untested": True, "rollout_percent": 100})
        self.wait()
        d.require(self.h.sql("SELECT count(*) FROM accounts") == "0", "fixture must have no business accounts")

    def run(self):
        self.setup()
        h = self.h
        old = {**h.app_facts(), "image": self.artifact["base_image_id"], "source": self.artifact["previous_source"],
               "has_readyz": True, "assets": h.assets(h.c["local_url"])}
        new = {**self.artifact, "has_readyz": True}
        before = {"data": h.data_identity(), "migrations": h.migrations(), "plugins": h.plugins()}
        h.verify(old)
        d.require(h.drain(), "fixture not idle")
        h.stop(self.root / "old-stop.log")
        h.start(new)
        check_runtime_uid(self.names["app"])
        for dependency in (self.names["postgres"], self.names["redis"]):
            d.run("docker", "pause", dependency)
            try:
                time.sleep(1.2)
                start = time.monotonic()
                h.curl(h.c["local_url"], "/readyz", expected=503, timeout=3)
                d.require(time.monotonic() - start < 4, "readiness deadline exceeded")
            finally:
                d.run("docker", "unpause", dependency)
            self.wait()
        processes = d.run("docker", "exec", self.names["app"], "ps", "-o", "pid,args").decode().splitlines()
        key = h.plugins()[0]["plugin_key"]
        pids = [line.split()[0] for line in processes if "/installed/" in line and key in line]
        d.require(len(pids) == 1 and pids[0].isdigit(), "fixture plugin process ambiguous")
        d.run("docker", "exec", self.names["app"], "kill", "-STOP", pids[0])
        try:
            time.sleep(1.2)
            h.curl(h.c["local_url"], "/readyz", expected=503)
        finally:
            d.run("docker", "exec", self.names["app"], "kill", "-CONT", pids[0])
        self.wait()
        h.stop(self.root / "relative-stop.log")
        self.environment["PLUGINS_DATA_DIR"] = "/app/data/plugins"
        d.save(self.root / "compose.json", self.compose)
        h.start(new)
        h.verify(new)
        h.stop(self.root / "candidate-stop.log")
        h.start(old)
        d.require({"data": h.data_identity(), "migrations": h.migrations(), "plugins": h.plugins()} == before,
                  "fixture round-trip changed persistent contract")
        result = {"candidate": new["image_id"], "previous": old["image_id"], "uid": 1000,
                "relative_and_absolute_paths": True, "dependency_faults": ["postgres", "redis", "plugin"],
                "roundtrip": "passed", "accounts": 0, "network": "internal", "production_mounts": False}
        if self.mode == "handoff":
            result["handoff"] = self.run_handoff(old, new)
        return result

    def run_handoff(self, old, new):
        h = self.h
        h.stop(self.root / "handoff-bootstrap-stop.log")
        runtime = self.data / ".handoff"
        runtime.mkdir(mode=0o700)
        os.chown(runtime, 1000, 1000)
        for name in ("owner.lock", "old.sock.lock", "new.sock.lock"):
            d.atomic(runtime / name, b"")
            os.chown(runtime / name, 1000, 1000)
        original = self.names["app"]
        candidate = original + "-candidate"
        self.environment.update(AUTO_SETUP="false", DEPLOYMENT_SOCKET="/app/data/.handoff/old.sock",
                                DEPLOYMENT_OWNER_LOCK="/app/data/.handoff/owner.lock")
        self.compose["services"]["candidate"] = {
            **self.compose["services"]["sub2api"], "image": new["image_id"], "container_name": candidate,
            "environment": {**self.environment, "DEPLOYMENT_SOCKET": "/app/data/.handoff/new.sock"}}
        d.save(self.root / "compose.json", self.compose)
        settings = h.sql("SELECT md5(coalesce(json_agg(s ORDER BY key)::text, '[]')) FROM settings s")
        before = {"migrations": h.migrations(), "plugins": d.plugin_contract(h.plugins())}
        h.compose("up", "-d", "--no-deps", "--pull", "never", "sub2api", "candidate")

        def control(container, action=None, expected=200):
            socket = "/app/data/.handoff/" + ("old" if container == original else "new") + ".sock"
            args = ["curl", "-q", "--noproxy", "*", "-sS", "--max-time", "35", "--unix-socket", socket]
            if action:
                args += ["-H", "Content-Type: application/json", "--data-binary", json.dumps({
                    "action": action, "boot_id": control(container)["boot_id"], "timeout_seconds": 30})]
            body, code = d.run("docker", "exec", container, *args, "-w", "\n%{http_code}",
                              "http://localhost/" + ("transition" if action else "status"), timeout=40).rsplit(b"\n", 1)
            d.require(int(code) == expected, "signed-plugin handoff failed: " + (action or "status"))
            return json.loads(body) if expected == 200 else None

        deadline = time.monotonic() + 90
        for container in (original, candidate):
            while True:
                try:
                    state = control(container)
                    d.require(state["phase"] == "standby" and state["core_ready"], "fixture not in standby")
                    break
                except Exception:
                    d.require(time.monotonic() < deadline, "managed fixture startup failed")
                    time.sleep(0.5)
        d.require(settings == h.sql("SELECT md5(coalesce(json_agg(s ORDER BY key)::text, '[]')) FROM settings s"),
                  "standby changed settings")
        d.require(before == {"migrations": h.migrations(), "plugins": d.plugin_contract(h.plugins())}, "standby changed contract")
        control(original, "serve")
        control(candidate, "serve", expected=409)
        control(original, "resume")
        h.verify(old)
        for source, target, artifact in ((original, candidate, new), (candidate, original, old)):
            control(source, "pause")
            state = control(target, "serve")
            d.require(state["phase"] == "serving_paused" and state["core_ready"], "target not core-ready with plugin paused")
            h.c["app_container"] = target
            h.curl(h.c["local_url"], "/health")
            h.curl(h.c["local_url"], "/readyz", expected=503)
            control(target, "resume")
            h.verify(artifact)
        d.require(before == {"migrations": h.migrations(), "plugins": d.plugin_contract(h.plugins())}, "handoff changed persistent contract")
        return {"shared_database": True, "standby_read_only": True, "exclusive_owner": True,
                "original_signed_plugin_roundtrip": True, "traffic_before_plugin_resume": True}

    def cleanup(self):
        # Only this freshly generated Compose project owns these resources.
        if self.started:
            try:
                self.h.compose("down", "--volumes", "--remove-orphans", timeout=120)
            except Exception:
                d.save(self.root / "cleanup.json", {"status": "failed"})
                raise
            d.save(self.root / "cleanup.json", {"status": "passed"})



def approved_inputs(config, host):
    manifest = Path(config["verification_manifest"])
    d.require(manifest.stat().st_uid == os.geteuid() and manifest.stat().st_mode & 0o077 == 0, "private verification manifest required")
    inputs = d.read(config["verification_manifest"])
    production_plugins = host.plugins()
    d.require(len(production_plugins) == 1 and production_plugins[0]["state"] == "enabled", "fixture supports one enabled approved plugin")
    d.require(d.plugin_contract(production_plugins) == inputs["production_plugins"], "plugin fixture approval drifted")
    for name in ("trust", "package", "plugin_config", "guard_evidence"):
        item = inputs[name]
        d.require(d.digest(item["path"]) == item["sha256"], "verification input changed: " + name)
    d.require(inputs["package"]["sha256"] == production_plugins[0]["artifact_sha256"], "fixture package differs from production")
    d.require(host.nginx_hash() == inputs["nginx_sha256"], "maintenance rules changed; repeat isolated guard acceptance")
    d.require(caller_inventory(host) == inputs["caller_inventory_sha256"], "direct caller inventory changed")
    for path, checksum in inputs["inventory_files"].items():
        d.require(d.digest(path) == checksum, "direct caller configuration changed")
    d.require(all(host.service_state(name) in (True, False) for name in host.c["pause_services"]), "unstable direct caller")
    if hasattr(host, "verify_ingress"):
        host.verify_ingress()
    else:
        ports = host.inspect(host.c["app_container"])["NetworkSettings"]["Ports"]
        d.require(ports and all(p["HostIp"] in ("127.0.0.1", "::1") for group in ports.values() if group for p in group), "application port exposed outside loopback")
    for origin in host.c["public_urls"]:
        host.curl(origin, "/__deploy_guard", expected=204)
    for guard in host.c.get("guard_checks", []):
        url = urlsplit(guard["url"])
        host.curl(url.scheme + "://" + url.netloc, url.path, expected=guard["open_status"])
    return inputs, production_plugins


def verify(job, config, host):
    inputs, production_plugins = approved_inputs(config, host)
    artifact = d.read(job / "build/artifact.json")
    fixture = Fixture(job, host, artifact, inputs)
    try:
        result = fixture.run()
    finally:
        fixture.cleanup()
    d.require(host.plugins() == production_plugins, "production plugin changed during verification")
    release = job / "release"
    d.evidence_template(release, Path(config["deployment_config"]), job / "build/artifact.json", host=host)
    receipt = d.read(release / "evidence.json")
    results = {"runtime_uid_paths": result, "plugin_rpc_existing_config": result, "startup_readiness": result,
               "compatible_rollback": result, "direct_callers_inventory": {"inventory": inputs["caller_inventory_sha256"], "files": inputs["inventory_files"]},
               "nginx_guard": {"configuration": inputs["nginx_sha256"], "installation_evidence": inputs["guard_evidence"]["sha256"],
                               "normal_guards": "passed", "maintenance_fault_injection": "reused_unchanged_installation"}}
    if host.c.get("mode") == "handoff":
        results["shared_database_handoff"] = result["handoff"]
    for name, result in results.items():
        check = receipt["checks"][name]
        d.save(release / check["log"], result)
        check.update(status="passed", sha256=d.digest(release / check["log"]))
    receipt["status"] = "passed"
    d.save(release / "evidence.json", receipt)

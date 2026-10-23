#!/usr/bin/env python3
"""Single-host application-only deployment. All mutating switches run in systemd."""
import argparse
from contextlib import contextmanager
import fcntl
import hashlib
from html.parser import HTMLParser
import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import time
from urllib.parse import urlsplit


ERROR_LOG = None


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def command_ok(result, executable):
    if result.returncode != 0 and ERROR_LOG and result.stderr:
        with ERROR_LOG.open("ab") as log:
            os.fchmod(log.fileno(), 0o600)
            log.write(("\n%s %s\n" % (time.time(), executable)).encode() + result.stderr)
    require(result.returncode == 0, "command failed: " + executable + "; inspect private command-errors.log/build.log")


def run(*args, timeout=60, stdout=None, merge_stderr=False):
    result = subprocess.run(args, stdout=stdout or subprocess.PIPE, stderr=subprocess.STDOUT if merge_stderr else subprocess.PIPE,
                            timeout=timeout, check=False)
    # Command output can contain secrets; keep it out of exception messages/journal.
    command_ok(result, args[0])
    return result.stdout


def digest(path):
    value = hashlib.sha256()
    with Path(path).open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            value.update(block)
    return value.hexdigest()


def atomic(path, data, mode=0o600):
    path = Path(path)
    require(not path.is_symlink(), "refusing symlink: " + str(path))
    old = path.stat() if path.exists() else None
    fd, name = tempfile.mkstemp(prefix="." + path.name, dir=path.parent)
    try:
        with os.fdopen(fd, "wb") as stream:
            os.fchmod(stream.fileno(), old.st_mode & 0o777 if old else mode)
            if old and os.geteuid() == 0:
                os.fchown(stream.fileno(), old.st_uid, old.st_gid)
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(name, path)
        directory = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        if os.path.exists(name):
            os.unlink(name)


def save(path, data):
    atomic(path, (json.dumps(data, sort_keys=True, indent=2) + "\n").encode())


def read(path):
    return json.loads(Path(path).read_text())


def image_env(data, image):
    require(re.fullmatch(r"[a-zA-Z0-9][a-zA-Z0-9._:/@-]*", image), "invalid image reference")
    result, count = re.subn(rb"^SUB2API_IMAGE=[^\r\n]*", b"SUB2API_IMAGE=" + image.encode(), data, flags=re.M)
    require(count == 1, ".env must have exactly one plain SUB2API_IMAGE binding")
    return result


def plugin_contract(plugins):
    # Runtime state may become 'error' during a failed start. Package/config/binding
    # identity must still match before restarting the same plugin with the old host.
    return [{k: v for k, v in plugin.items() if k != "state"} for plugin in plugins]


class EntryAssets(HTMLParser):
    def __init__(self, html):
        super().__init__()
        self.paths = set()
        self.feed(html)

    def handle_starttag(self, tag, attrs):
        attrs = dict(attrs)
        path = attrs.get("src") if tag == "script" else attrs.get("href") if tag == "link" else None
        if path and path.startswith("/assets/"):
            self.paths.add(path)


def validate_config(c):
    for key in ("deploy_dir", "compose_file", "env_file", "admin_header_file", "maintenance_file"):
        require(Path(c[key]).is_absolute(), "absolute path required: " + key)
    for path in c["snapshot_files"]:
        require(Path(path).is_absolute(), "absolute snapshot path required")
    require(Path(c["maintenance_file"]).parent == Path(c["deploy_dir"]), "maintenance file must be in deploy directory")
    require(c["public_urls"] and all(urlsplit(u).scheme == "https" and not urlsplit(u).username
                                   and urlsplit(u).path in ("", "/") for u in c["public_urls"]), "HTTPS origins required")
    for check in c.get("guard_checks", []):
        url = urlsplit(check["url"])
        require(url.scheme == "http" and url.hostname in ("127.0.0.1", "::1") and not url.username
                and not url.query and not url.fragment and url.path.startswith("/"), "extra guards must be loopback HTTP")
        require(check["open_status"] in (200, 204, 301, 403), "unexpected open guard status")
    local = urlsplit(c["local_url"])
    require(local.scheme == "http" and local.hostname in ("127.0.0.1", "::1"), "local URL must be loopback")
    require(local.port == c["app_port"], "local URL/port mismatch")
    require(1 <= c["drain_seconds"] <= 30 and 1 <= c["ready_seconds"] <= 120, "unbounded maintenance timeout")
    require(60 <= c["backup_max_age_seconds"] <= 7200, "backup age must not exceed two hours")
    require(isinstance(c["previous_has_readyz"], bool), "explicit bootstrap readiness mode required")
    require(all(re.fullmatch(r"[A-Za-z0-9_.@-]+\.service", s) for s in c["pause_services"]), "invalid service name")
    require(len(c["pause_services"]) == len(set(c["pause_services"])), "duplicate service")


@contextmanager
def locked(c):
    with open(Path(c["deploy_dir"]) / ".deploy.lock", "a+") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        yield


class Host:
    def __init__(self, config):
        validate_config(config)
        self.c = config
        self.marker = Path(config["maintenance_file"])
        self.pending = Path(config["deploy_dir"]) / ".fast-deploy-pending"

    def inspect(self, name, image=False):
        command = ("image", "inspect") if image else ("inspect",)
        return json.loads(run("docker", *command, name))[0]

    def compose(self, *args, timeout=60):
        # An inherited SUB2API_IMAGE/COMPOSE_FILE must not override the reviewed file.
        env = {k: v for k, v in os.environ.items() if k in ("PATH", "HOME", "DOCKER_CONFIG", "XDG_RUNTIME_DIR")}
        result = subprocess.run(["docker", "compose", "--project-directory", self.c["deploy_dir"],
                                 "--env-file", self.c["env_file"], "-f", self.c["compose_file"], *args],
                                env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout)
        command_ok(result, "docker compose")
        return result.stdout

    def sql(self, query):
        return run("docker", "exec", "-e", "PGOPTIONS=-c default_transaction_read_only=on -c statement_timeout=5000",
                   self.c["postgres_container"], "psql", "-XAt", "-v", "ON_ERROR_STOP=1",
                   "-U", self.c["database_user"], "-d", self.c["database_name"], "-c", query).decode().strip()

    def migrations(self):
        return dict(line.split("|", 1) for line in self.sql("SELECT filename, checksum FROM schema_migrations ORDER BY filename").splitlines())

    def plugins(self):
        return json.loads(self.sql("""SELECT coalesce(json_agg(t ORDER BY id), '[]') FROM (
          SELECT id, plugin_key, version, binary_sha256, signature_status, state,
            encode(sha256(convert_to(config_encrypted, 'UTF8')), 'hex') AS config_sha256,
            encode(sha256(artifact_data), 'hex') AS artifact_sha256,
            (SELECT coalesce(json_agg(json_build_array(capability,platform,account_type,enabled,rollout_percent)
               ORDER BY capability,platform,account_type),'[]') FROM sub2api_plugin_bindings b WHERE b.plugin_id=p.id) AS bindings
          FROM sub2api_plugin_installations p) t"""))

    def curl(self, origin, path, admin=False, expected=200, timeout=5):
        require(timeout > 0, "request deadline reached")
        args = ["curl", "-q", "--noproxy", "*", "--silent", "--show-error", "--max-time", str(timeout),
                "--header", "Connection: close", "--write-out", "\n%{http_code}"]
        if admin:
            require(origin == self.c["local_url"], "admin credentials must stay on loopback")
            args += ["--header", "@" + self.c["admin_header_file"]]
        body, code = run(*args, origin.rstrip("/") + path, timeout=timeout + 0.1).rsplit(b"\n", 1)
        require(int(code) == expected, "HTTP check failed: " + path + " status=" + code.decode())
        return body

    def api(self, path, timeout=5):
        result = json.loads(self.curl(self.c["local_url"], path, admin=True, timeout=timeout))
        require(result.get("code") == 0 and "data" in result, "admin API unavailable")
        return result["data"]

    def data_identity(self):
        result = {}
        for key in ("postgres_container", "redis_container"):
            info = self.inspect(self.c[key])
            require(info["State"].get("Health", {}).get("Status") == "healthy", "data service unhealthy")
            result[key] = {"id": info["Id"], "started": info["State"]["StartedAt"], "image": info["Image"]}
        return result

    def nginx_hash(self):
        return hashlib.sha256(run("nginx", "-T")).hexdigest()

    def config_hashes(self):
        return {p: digest(p) for p in [self.c["compose_file"], self.c["admin_header_file"], *self.c["snapshot_files"]]}

    def compose_hash(self):
        config = json.loads(self.compose("config", "--format", "json"))
        config["services"][self.c["app_service"]]["image"] = "normalized"
        return hashlib.sha256(json.dumps(config, sort_keys=True).encode()).hexdigest()

    def check_applied_compose(self):
        hashes = self.compose("config", "--hash", self.c["app_service"]).decode().split()
        require(len(hashes) == 2 and hashes[0] == self.c["app_service"], "Compose config hash unavailable")
        labels = self.inspect(self.c["app_container"])["Config"]["Labels"]
        require(labels.get("com.docker.compose.config-hash") == hashes[1],
                "running Compose configuration differs; review pending changes or CLI version before deploying")

    def service_state(self, name):
        state = run("systemctl", "show", name, "--property=LoadState,ActiveState", "--value").decode().splitlines()
        require("loaded" in state and ("active" in state or "inactive" in state), "service is not stable: " + name)
        return "active" in state

    def pause(self, name):
        run("systemctl", "stop", name, timeout=30)
        require(not self.service_state(name), "direct caller did not stop")

    def restore(self, name):
        run("systemctl", "start", name, timeout=30)
        require(self.service_state(name), "direct caller did not resume")

    def gate(self, closed):
        if closed:
            atomic(self.marker, b"maintenance\n", 0o644)
        else:
            self.marker.unlink(missing_ok=True)
        for origin in self.c["public_urls"]:
            self.curl(origin, "/__deploy_guard", expected=503 if closed else 204)
        for check in self.c.get("guard_checks", []):
            url = urlsplit(check["url"])
            self.curl(url.scheme + "://" + url.netloc, url.path,
                      expected=503 if closed else check["open_status"])

    def drain(self):
        deadline, stable = time.monotonic() + self.c["drain_seconds"], 0
        while time.monotonic() < deadline:
            counts = []
            for path, field in (("concurrency", "platform"), ("user-concurrency", "user")):
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    return False
                try:
                    data = self.api("/api/v1/admin/ops/" + path, timeout=remaining)
                except (subprocess.TimeoutExpired, RuntimeError):
                    return False
                require(data.get("enabled") is True and isinstance(data.get(field), dict), "drain metrics disabled")
                counts += [sum(row["current_in_use"] + row["waiting_in_queue"] for row in data[field].values())]
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                return False
            try:
                raw = run("docker", "exec", self.c["app_container"], "cat", "/proc/net/tcp", "/proc/net/tcp6", timeout=remaining).decode()
            except subprocess.TimeoutExpired:
                return False
            port = ":%04X" % self.c["app_port"]
            connections = sum(len(p) > 3 and p[1].endswith(port) and p[3] == "01" for p in map(str.split, raw.splitlines()))
            print(json.dumps({"drain": counts, "connections": connections}), flush=True)
            stable = stable + 1 if counts == [0, 0] and connections == 0 else 0
            if stable >= 3 and time.monotonic() <= deadline:
                return True
            time.sleep(min(0.25, max(0, deadline - time.monotonic())))
        return False

    def app_facts(self):
        info = self.inspect(self.c["app_container"])
        require(info["State"]["Running"] and not info["State"]["Restarting"], "application not running")
        binary = run("docker", "exec", self.c["app_container"], "sha256sum", "/app/sub2api").decode().split()[0]
        raw_version = run("docker", "exec", self.c["app_container"], "/app/sub2api", "-version").decode()
        match = re.search(r"Sub2API\s+v?[0-9][A-Za-z0-9.+-]*\s+\(commit:\s*[0-9a-f]{40},\s*built:\s*[^)\r\n]*\)", raw_version)
        require(match, "application version identity unavailable")
        version = match.group(0)
        return {"image_id": info["Image"], "binary_sha256": binary, "version": version}

    def assets(self, origin, expected=None):
        paths = EntryAssets(self.curl(origin, "/model-plaza").decode()).paths
        require(paths and any(p.endswith(".js") for p in paths), "missing frontend entry")
        values = {p: hashlib.sha256(self.curl(origin, p)).hexdigest() for p in sorted(paths)}
        if expected is not None:
            require(all(expected.get(p) == value for p, value in values.items()), "frontend identity mismatch")
        return values

    def verify(self, target, public=False):
        origin = self.c["local_url"]
        facts = self.app_facts()
        require(facts["image_id"] == target["image_id"] and facts["binary_sha256"] == target["binary_sha256"], "application artifact mismatch")
        require(target["source"] in facts["version"], "running source mismatch")
        require(json.loads(self.curl(origin, "/health"))["status"] == "ok", "liveness failed")
        if target["has_readyz"]:
            require(json.loads(self.curl(origin, "/readyz"))["status"] == "ready", "readiness failed")
        # Also needed for bootstrap rollback to a binary without /readyz.
        for plugin in self.plugins():
            if any(b[3] for b in plugin["bindings"]):
                require(plugin["state"] == "enabled" and plugin["signature_status"] == "trusted", "plugin not trusted/enabled")
                require(self.api("/api/v1/admin/plugins/%s/status" % plugin["id"])["healthy"] is True, "plugin unhealthy")
        self.curl(origin, "/api/v1/admin/usage?page=1&page_size=1", expected=401)
        self.api("/api/v1/admin/usage?page=1&page_size=1")
        self.assets(origin, target["assets"])
        if public:
            for origin in self.c["public_urls"]:
                require(json.loads(self.curl(origin, "/health"))["status"] == "ok", "public liveness failed")
                self.assets(origin, target["assets"])

    def stop(self, log):
        since = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
        self.compose("stop", "-t", "-1", self.c["app_service"], timeout=90)
        info = self.inspect(self.c["app_container"])
        result = subprocess.run(["docker", "logs", "--since", since, self.c["app_container"]],
                                stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=15, check=True)
        atomic(log, result.stdout)
        require(not info["State"]["Running"] and not info["State"]["OOMKilled"] and info["State"]["ExitCode"] == 0, "unclean stop")
        require(b"[Cleanup] All cleanup steps completed" in result.stdout and
                not re.search(rb"Server forced to shutdown|\[Cleanup\].*(failed|timed out)", result.stdout), "shutdown cleanup incomplete")

    def start(self, target):
        require(self.inspect(target["image"], image=True)["Id"] == target["image_id"], "image tag changed")
        path = Path(self.c["env_file"])
        atomic(path, image_env(path.read_bytes(), target["image"]))
        self.compose("config", "--quiet")
        self.compose("up", "-d", "--no-deps", "--no-build", "--pull", "never", self.c["app_service"])
        deadline = time.monotonic() + self.c["ready_seconds"]
        while time.monotonic() < deadline:
            try:
                self.verify(target)
                return
            except (RuntimeError, subprocess.TimeoutExpired):
                time.sleep(1)
        raise RuntimeError("application acceptance deadline reached")

    def unchanged(self, prepared):
        require(self.config_hashes() == prepared["config_hashes"], "configuration changed")
        require(self.compose_hash() == prepared["compose_sha256"], "rendered Compose configuration changed")
        require(self.nginx_hash() == prepared["nginx_sha256"], "nginx configuration changed")
        require(self.data_identity() == prepared["data_identity"], "database/redis identity or start time changed")
        require(self.migrations() == prepared["migrations"], "migration history changed; automatic rollback prohibited")
        require(plugin_contract(self.plugins()) == plugin_contract(prepared["plugins"]),
                "plugin package/config/binding changed; automatic rollback prohibited")
        require(image_env(Path(self.c["env_file"]).read_bytes(), "normalized") ==
                image_env((Path(prepared["release"]) / "before.env").read_bytes(), "normalized"), "non-image .env changes")


def deployment_host(config):
    mode = config.get("mode", "single")
    require(mode in ("single", "handoff"), "unknown deployment mode")
    if mode == "handoff":
        from handoff_deploy import Host as HandoffHost
        return HandoffHost(config)
    return Host(config)


def deployment_switch(release, attempt):
    if read(release / "config.json").get("mode") == "handoff":
        from handoff_deploy import Switch as HandoffSwitch
        return HandoffSwitch(release, attempt)
    return Switch(release, attempt)


def evidence(release, artifact, host):
    receipt = read(release / "evidence.json")
    require(receipt["status"] == "passed", "isolated rehearsal not passed")
    for key in ("source", "previous_source", "image_id", "base_image_id", "binary_sha256"):
        require(receipt[key] == artifact[key], "evidence belongs to another artifact: " + key)
    require(receipt["config_sha256"] == digest(release / "config.json"), "evidence configuration mismatch")
    require(receipt["plugins"] == host.plugins(), "evidence plugin mismatch")
    names = ["runtime_uid_paths", "plugin_rpc_existing_config", "startup_readiness", "compatible_rollback",
             "direct_callers_inventory", "nginx_guard"]
    if host.c.get("mode") == "handoff":
        names.append("shared_database_handoff")
    for name in names:
        item = receipt["checks"][name]
        path = (release / item["log"]).resolve()
        require(release in path.parents and path.is_file() and path.stat().st_size > 0, "missing rehearsal log: " + name)
        require(item["status"] == "passed" and digest(path) == item["sha256"], "invalid rehearsal evidence: " + name)


def evidence_template(release, config_path, artifact_path, host=None):
    require(release.is_dir() and release.stat().st_mode & 0o077 == 0, "pre-create private release directory (0700)")
    require(not (release / "evidence.json").exists() and not (release / "prepared.json").exists(), "refusing to overwrite evidence")
    c, artifact = read(config_path), read(artifact_path)
    host = host or deployment_host(c)
    save(release / "config.json", c)
    receipt = {k: artifact[k] for k in ("source", "previous_source", "image_id", "base_image_id", "binary_sha256")}
    receipt.update(status="not_run", config_sha256=digest(release / "config.json"), plugins=host.plugins(), checks={
        name: {"status": "not_run", "log": name + ".log", "sha256": ""} for name in (
            "runtime_uid_paths", "plugin_rpc_existing_config", "startup_readiness", "compatible_rollback",
            "direct_callers_inventory", "nginx_guard")})
    if c.get("mode") == "handoff":
        receipt["checks"]["shared_database_handoff"] = {"status": "not_run", "log": "shared_database_handoff.log", "sha256": ""}
    save(release / "evidence.json", receipt)
    print("Evidence template only: run the isolated checks and record actual logs before prepare")


def prepare(release, config_path, artifact_path):
    require(release.is_dir() and release.stat().st_mode & 0o077 == 0, "pre-create private release directory (0700)")
    require(not (release / "prepared.json").exists(), "release already prepared; use a new directory")
    c, artifact = read(config_path), read(artifact_path)
    host = deployment_host(c)
    active = host.c
    with locked(c):
        require(not host.marker.exists() and not host.pending.exists(), "maintenance/recovery pending")
        require(not (release / "preparation-started.json").exists(), "preparation already attempted; preserve it and use a new directory")
        save(release / "config.json", c)
        save(release / "artifact.json", artifact)
        require(Path(c["admin_header_file"]).stat().st_mode & 0o077 == 0, "admin header file must be private")
        evidence(release, artifact, host)
        old = host.app_facts()
        require(old["image_id"] == artifact["base_image_id"] and artifact["previous_source"] in old["version"], "baseline differs from build")
        require(host.inspect(artifact["image"], image=True)["Id"] == artifact["image_id"], "candidate tag changed")
        old_env = Path(active["env_file"]).read_bytes()
        image_env(old_env, "check")
        image = re.findall(rb"^SUB2API_IMAGE=([^\r\n]*)", old_env, re.M)[0].decode()
        require(host.inspect(image, image=True)["Id"] == old["image_id"], "old image binding mismatch")
        compose = json.loads(host.compose("config", "--format", "json"))
        require(compose["services"][active["app_service"]]["image"] == image, "Compose does not use SUB2API_IMAGE")
        require(host.compose("ps", "--all", "--quiet", active["app_service"]).decode().strip() ==
                host.inspect(active["app_container"])["Id"], "Compose points at a different application")
        host.check_applied_compose()
        migrations = host.migrations()
        for name, checksum in artifact["migrations"].items():
            recorded = name if name in migrations else c["migration_aliases"].get(name, name)
            require(migrations.get(recorded) == checksum, "pending or changed migration: " + name)
        old.update(image=image, source=artifact["previous_source"], has_readyz=c["previous_has_readyz"], assets=host.assets(active["local_url"]))
        new = {**artifact, "has_readyz": True}
        host.verify(old, public=True)
        host.gate(False)
        require(all(host.service_state(s) in (True, False) for s in c["pause_services"]), "caller service unavailable")
        atomic(release / "before.env", old_env)
        hashes = host.config_hashes()
        for index, path in enumerate(hashes):
            atomic(release / ("config-snapshot-%d" % index), Path(path).read_bytes())
        before = {"release": str(release), "previous": old, "candidate": new,
                  "config_hashes": hashes, "compose_sha256": host.compose_hash(), "nginx_sha256": host.nginx_hash(),
                  "data_identity": host.data_identity(), "migrations": migrations, "plugins": host.plugins()}
        if c.get("mode") == "handoff":
            host.verify_ingress()
            protocol = host.control(host.selected()).get("handoff_protocol", 1)
            if protocol == 2:
                receipt = read(release / "evidence.json")
                rehearsal = read(release / receipt["checks"]["shared_database_handoff"]["log"])
                require(rehearsal.get("live_handoff") is True, "live handoff rehearsal missing")
                host.verify_live_ingress()
            before["handoff"] = {"original_slot": host.selected(), "env_hashes": host.env_hashes(), "protocol": protocol}
        save(release / "preparation-started.json", {"started_at": time.time(), "source": artifact["source"]})
        with (release / "postgres.dump").open("wb") as dump:
            run("docker", "exec", c["postgres_container"], "pg_dump", "-Fc", "-U", c["database_user"],
                "-d", c["database_name"], timeout=1800, stdout=dump)
        # Match pg_restore to the running database without copying a multi-GB file into it.
        with (release / "postgres.dump").open("rb") as dump, (release / "backup-list.txt").open("wb") as listing:
            subprocess.run(["docker", "exec", "-i", c["postgres_container"], "pg_restore", "--list"],
                           stdin=dump, stdout=listing, stderr=subprocess.PIPE, timeout=180, check=True)
        require((release / "postgres.dump").stat().st_size > 0 and (release / "backup-list.txt").stat().st_size > 0, "empty backup")
        before["backup_at"] = time.time()
        before["backup_sha256"] = digest(release / "postgres.dump")
        tools = release / "tools"
        tools.mkdir(mode=0o700)
        shutil.copyfile(Path(__file__), tools / "fast_deploy.py")
        if c.get("mode") == "handoff":
            shutil.copyfile(Path(__file__).with_name("handoff_deploy.py"), tools / "handoff_deploy.py")
        before["sealed_files"] = {str(p.relative_to(release)): digest(p) for p in
                                  [release / "config.json", release / "artifact.json", release / "evidence.json",
                                   release / "before.env", release / "backup-list.txt", tools / "fast_deploy.py"]}
        if c.get("mode") == "handoff":
            before["sealed_files"]["tools/handoff_deploy.py"] = digest(tools / "handoff_deploy.py")
        host.unchanged(before)
        save(release / "prepared.json", before)
    print("PREPARED: no maintenance, restart or deployment performed")


TERMINAL = {"ACCEPTED", "ROLLED_BACK", "DEFERRED", "RECOVERED", "PREFLIGHT_FAILED", "LAUNCH_FAILED"}


class Switch:
    def __init__(self, release, attempt, host=None):
        self.release, self.attempt = release, attempt
        self.p = read(release / "prepared.json")
        self.h = host or Host(read(release / "config.json"))
        self.s = read(attempt / "state.json")
        self.original = self.p["previous" if self.s["mode"] == "activate" else "candidate"]
        self.target = self.p["candidate" if self.s["mode"] == "activate" else "previous"]

    def state(self, phase, **fields):
        self.s.update(phase=phase, updated_at=time.time(), **fields)
        save(self.attempt / "state.json", self.s)
        print(phase, flush=True)

    def preflight(self):
        for name, value in self.p["sealed_files"].items():
            require(digest(self.release / name) == value, "prepared file changed: " + name)
        require(0 <= time.time() - self.p["backup_at"] <= self.h.c["backup_max_age_seconds"], "backup expired; prepare a new release")
        require(digest(self.release / "postgres.dump") == self.p["backup_sha256"], "backup changed")
        evidence(self.release, read(self.release / "artifact.json"), self.h)
        for target in (self.original, self.target):
            require(self.h.inspect(target["image"], image=True)["Id"] == target["image_id"], "image tag changed")
        self.h.unchanged(self.p)
        require(not self.h.marker.exists() and not self.h.pending.exists(), "another deployment needs recovery")
        require(Path(self.h.c["env_file"]).read_bytes() == image_env((self.release / "before.env").read_bytes(), self.original["image"]), "unexpected current image binding")
        self.h.verify(self.original, public=True)
        self.h.check_applied_compose()
        self.h.gate(False)

    def finish(self, phase, target):
        self.h.unchanged(self.p)
        self.h.verify(target)
        self.h.gate(False)
        self.state("PUBLIC_VERIFY", public_opened=True)
        self.h.verify(target, public=True)
        for name, was_active in self.s.get("services", {}).items():
            if was_active:
                self.h.restore(name)
        self.h.unchanged(self.p)
        self.state(phase)
        self.h.pending.unlink(missing_ok=True)

    def execute(self):
        try:
            self.preflight()
        except Exception:
            self.state("PREFLIGHT_FAILED")
            raise
        services = {s: self.h.service_state(s) for s in self.h.c["pause_services"]}
        self.state("PAUSING", services=services, touched=True, public_opened=False)
        atomic(self.h.pending, str(self.attempt).encode())
        try:
            for name, active in services.items():
                if active:
                    self.h.pause(name)
                require(not self.h.service_state(name), "direct caller is still active")
            self.h.gate(True)
            self.state("DRAINING")
            if not self.h.drain():
                self.finish("DEFERRED", self.original)
                return
            self.h.unchanged(self.p)
            self.state("STOPPING", stop_started=True)
            self.h.stop(self.attempt / "shutdown.log")
            self.state("STARTING", stop_completed=True)
            self.h.start(self.target)
            self.finish("ACCEPTED" if self.s["mode"] == "activate" else "ROLLED_BACK", self.target)
        except Exception as error:
            self.state("RECOVERING", error_type=type(error).__name__, error=str(error))
            self.recover()
            raise

    def recover(self):
        if self.s["phase"] in TERMINAL:
            if self.h.pending.exists() and self.h.pending.read_text() == str(self.attempt) and not self.h.marker.exists():
                self.h.pending.unlink()
            return
        if not self.s.get("touched"):
            self.state("PREFLIGHT_FAILED")
            return
        if self.s["phase"] == "PAUSING" and not self.h.pending.exists():
            self.state("PREFLIGHT_FAILED")
            return
        try:
            require(self.h.pending.read_text() == str(self.attempt), "recovery owner mismatch")
            self.h.gate(True)
            for name, active in self.s["services"].items():
                if active:
                    self.h.pause(name)
            self.h.unchanged(self.p)
            info = self.h.inspect(self.h.c["app_container"])
            require(info["Image"] in (self.original["image_id"], self.target["image_id"]), "unknown application image")
            if info["State"]["Running"]:
                require(not (self.s.get("stop_started") and not self.s.get("stop_completed")), "stop result unresolved")
                require(not (self.s.get("recovery_stop_started") and not self.s.get("recovery_stop_completed")),
                        "recovery stop result unresolved")
                if info["Image"] != self.original["image_id"]:
                    require(self.h.drain(), "candidate requests still active; leave maintenance for review")
                    self.state("RECOVERING", recovery_stop_started=True, recovery_stop_completed=False)
                    self.h.stop(self.attempt / "recovery-shutdown.log")
                    self.state("RECOVERING", recovery_stop_completed=True)
                    self.h.start(self.original)
            else:
                self.h.start(self.original)
            self.finish("RECOVERED", self.original)
        except Exception as error:
            # Never resume direct callers into an unknown host/plugin pairing.
            atomic(self.h.marker, b"recovery required\n", 0o644)
            self.state("NEEDS_ATTENTION", recovery_error_type=type(error).__name__, recovery_error=str(error))
            raise


def launch(release, mode):
    c = read(release / "config.json")
    with locked(c):
        host = deployment_host(c)
        require(not host.pending.exists(), "previous task needs recovery")
        attempt = release / mode
        require(not attempt.exists(), "attempt already exists; inspect status, never replay")
        if mode == "rollback":
            require(read(release / "activate/state.json")["phase"] == "ACCEPTED", "only accepted releases can be rolled back")
        attempt.mkdir(mode=0o700)
        unit = "sub2api-fast-" + hashlib.sha256(str(attempt).encode()).hexdigest()[:16]
        save(attempt / "state.json", {"phase": "QUEUED", "mode": mode, "unit": unit, "created_at": time.time()})
        script = release / "tools/fast_deploy.py"
        # systemd parses its own command syntax. Restrict these paths, do not shell-quote them.
        require(re.fullmatch(r"/[A-Za-z0-9_./-]+", str(release)), "release path must not contain spaces or metacharacters")
        require(digest(script) == read(release / "prepared.json")["sealed_files"]["tools/fast_deploy.py"], "runner changed")
        try:
            run("systemd-run", "--unit=" + unit, "--property=Type=exec", "--property=UMask=0077",
                "--property=TimeoutStopSec=infinity", "--property=KillMode=control-group",
                "--property=StandardOutput=append:" + str(attempt / "task.log"),
                "--property=StandardError=append:" + str(attempt / "task.log"),
                "--property=ExecStopPost=/usr/bin/python3 " + str(script) + " _recover --release " + str(release) + " --attempt " + mode,
                "/usr/bin/python3", str(script), "_run", "--release", str(release), "--attempt", mode)
        except Exception:
            save(attempt / "state.json", {"phase": "LAUNCH_FAILED", "mode": mode, "unit": unit})
            raise
    print(unit + "; inspect " + str(attempt / "state.json"))


def main():
    global ERROR_LOG
    os.umask(0o077)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("evidence-template", "prepare", "activate", "rollback", "status", "_run", "_recover"))
    parser.add_argument("--release", required=True)
    parser.add_argument("--config")
    parser.add_argument("--artifact")
    parser.add_argument("--attempt", choices=("activate", "rollback"))
    args = parser.parse_args()
    release = Path(args.release).resolve()
    log_dir = release / args.attempt if args.attempt else release
    if log_dir.is_dir():
        ERROR_LOG = log_dir / "command-errors.log"
    if args.command == "status":
        if (release / "prepared.json").exists():
            prepared = read(release / "prepared.json")
            config = read(release / "config.json")
            age = time.time() - prepared["backup_at"]
            window = ("CLOCK_INVALID" if age < 0 else "CURRENT" if age <= config["backup_max_age_seconds"] else "EXPIRED")
            expires = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(prepared["backup_at"] + config["backup_max_age_seconds"]))
            print("PREPARATION_SNAPSHOT source=" + prepared["candidate"]["source"] + " image=" + prepared["candidate"]["image_id"])
            print("BACKUP_WINDOW=" + window + " expires_at=" + expires)
        else:
            print("NOT_PREPARED")
        print("LIVE_STATE_NOT_CHECKED: status is read-only; activation/rollback still require full preflight")
        for path in sorted(release.glob("*/state.json")):
            print(str(path) + "\n" + path.read_text())
        return
    if args.command in ("prepare", "evidence-template"):
        require(args.config and args.artifact, "--config and --artifact required")
        operation = prepare if args.command == "prepare" else evidence_template
        operation(release, Path(args.config), Path(args.artifact))
    elif args.command in ("activate", "rollback"):
        launch(release, args.command)
    else:
        require(args.attempt, "attempt required")
        c = read(release / "config.json")
        # launch holds the same lock briefly; workers wait, other launches do not.
        with open(Path(c["deploy_dir"]) / ".deploy.lock", "a+") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX)
            switch = deployment_switch(release, release / args.attempt)
            if args.command == "_recover":
                switch.recover()
            else:
                require(switch.s["phase"] == "QUEUED", "attempt cannot be replayed")
                signal.signal(signal.SIGTERM, lambda *_: sys.exit(143))
                switch.execute()


if __name__ == "__main__":
    # Handoff imports this module; share its error log and command helpers.
    sys.modules.setdefault("fast_deploy", sys.modules[__name__])
    main()

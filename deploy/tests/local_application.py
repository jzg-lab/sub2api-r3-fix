#!/usr/bin/env python3
"""Local shared-database handoff checks; no signed plugin or release acceptance."""

import io
import json
import os
from pathlib import Path
import secrets
import subprocess
import tarfile
import tempfile
import time
import uuid

ROOT = Path(__file__).resolve().parents[2]
CONTEXT = "colima-sub2api-verify"


def run(args, *, cwd=None, env=None, data=None, timeout=180):
    result = subprocess.run(args, cwd=cwd, env=env, input=data,
                            stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=timeout)
    if result.returncode:
        raise RuntimeError(f"{args[0]} failed ({result.returncode}):\n{result.stdout.decode(errors='replace')[-6000:]}")
    return result.stdout.decode().strip()


BASE = "nginx:1.28-alpine@sha256:a8b39bd9cf0f83869a2162827a0caf6137ddf759d50a171451b335cecc87d236"


def main():
    docker = ["docker", "--context", CONTEXT]
    context = json.loads(run(docker + ["context", "inspect", CONTEXT]))[0]
    expected = f"unix://{Path.home()}/.colima/sub2api-verify/docker.sock"
    if context["Endpoints"]["docker"]["Host"] != expected:
        raise RuntimeError("Refusing any endpoint except the dedicated local test VM")
    if run(docker + ["info", "--format", "{{.OSType}}/{{.Architecture}}"]) != "linux/aarch64":
        raise RuntimeError("This fixture requires local Linux/ARM64")
    images = {name: json.loads(run(docker + ["image", "inspect", tag]))[0]["Id"]
              for name, tag in {
                  "postgres": "postgres:18-alpine@sha256:77f585114c32fbca283dc835b0596f4e52b51b4c6662d7810b2f4084f60a1873",
                  "redis": "redis:8-alpine@sha256:3811787313eba226a2ef38658c6ccb91cd5e110edc89c37767de373120a0e5a0",
              }.items()}
    print("Local dependency images: " + json.dumps(images), flush=True)
    token = "sub2api-local-app-" + uuid.uuid4().hex[:12]
    image, network, volume = token + ":test", token, token
    containers = []
    started = time.monotonic()
    password = secrets.token_hex(24)
    config = {
        "database": {"host": "postgres", "port": 5432, "user": "fixture", "password": password,
                     "dbname": "fixture", "sslmode": "disable", "max_open_conns": 12, "max_idle_conns": 4},
        "redis": {"host": "redis", "port": 6379},
        "server": {"host": "0.0.0.0", "port": 8080, "mode": "release"},
        "jwt": {"secret": secrets.token_hex(32), "expire_hour": 24},
        "totp": {"encryption_key": secrets.token_hex(32)},
        "log": {"output": {"to_file": False, "to_stdout": True}},
        "pricing": {"remote_url": "", "hash_url": "", "data_dir": "/shared",
                    "fallback_file": "/shared/model_pricing.json"},
        "plugins": {"data_dir": "/app/data/plugins"},
    }

    def start(name, args, target=image, command=None):
        name = token + "-" + name
        containers.append(name)
        run(docker + ["run", "-d", "--name", name, "--network", network,
                      "--network-alias", name.removeprefix(token + "-"),
                      "--pids-limit=256", "--cpus=1", "--memory=1g",
                      *args, target, *(command or [])])
        return name

    def app(name, managed=False):
        variables = {"DATA_DIR": "/app/data"}
        if managed:
            variables.update(DEPLOYMENT_SOCKET="/shared/" + name + ".sock", DEPLOYMENT_OWNER_LOCK="/shared/owner.lock")
        env_args = [item for k, v in variables.items() for item in ("-e", k + "=" + v)]
        return start(name, ["--user=10001:10001", "--read-only", "--cap-drop=ALL",
                           "--security-opt=no-new-privileges", "--tmpfs", "/tmp:rw,nosuid,nodev,size=128m",
                           "--tmpfs", "/app/data/plugins:rw,nosuid,nodev,uid=10001,gid=10001,size=64m",
                           "--mount", f"type=volume,src={volume},dst=/shared", *env_args])

    def sql(query):
        return run(docker + ["exec", pg, "psql", "-U", "fixture", "-d", "fixture", "-At", "-v", "ON_ERROR_STOP=1", "-c", query])

    def status(name, path="/readyz"):
        return run(docker + ["exec", name, "curl", "-q", "--noproxy", "*", "-sS", "--max-time", "4",
                            "-o", "/dev/null", "-w", "%{http_code}", "http://127.0.0.1:8080" + path], timeout=7)

    def wait_ready(name):
        deadline = time.monotonic() + 60
        while time.monotonic() < deadline:
            if run(docker + ["inspect", "-f", "{{.State.Running}}", name]) != "true":
                raise RuntimeError("application exited: " + run(docker + ["logs", name])[-6000:])
            try:
                if status(name) == "200":
                    return
            except RuntimeError:
                pass
            time.sleep(0.25)
        raise RuntimeError("application readiness timed out: " + name)

    def snapshot(name):
        return {
            "migrations": sql("SELECT coalesce(json_agg(m ORDER BY filename)::text, '[]') FROM schema_migrations m"),
            "secrets": sql("SELECT md5(coalesce(json_agg(s ORDER BY key)::text, '[]')) FROM security_secrets s"),
            "settings": sql("SELECT md5(coalesce(json_agg(s ORDER BY key)::text, '[]')) FROM settings s"),
            "price": run(docker + ["exec", name, "sha256sum", "/shared/model_pricing.json"]),
            "config": run(docker + ["exec", name, "sha256sum", "/app/data/config.yaml"]),
        }

    def control(name, action=None, expected=200):
        socket = "/shared/" + name.removeprefix(token + "-") + ".sock"
        args = ["curl", "-q", "--noproxy", "*", "-sS", "--max-time", "35", "--unix-socket", socket]
        if action:
            boot = control(name)["boot_id"]
            args += ["-H", "Content-Type: application/json", "--data-binary",
                     json.dumps({"action": action, "boot_id": boot, "timeout_seconds": 30})]
        response = run(docker + ["exec", name, *args, "-w", "\n%{http_code}",
                                "http://localhost/" + ("transition" if action else "status")], timeout=40)
        body, code = response.rsplit("\n", 1)
        if int(code) != expected:
            raise RuntimeError("unexpected runtime response: " + response)
        return json.loads(body) if expected == 200 else None

    def standby(name):
        deadline = time.monotonic() + 60
        while time.monotonic() < deadline:
            try:
                state = control(name)
                if state["phase"] == "standby" and state["core_ready"]:
                    if status(name) != "503":
                        raise RuntimeError("standby admits traffic")
                    return
            except RuntimeError:
                pass
            time.sleep(0.25)
        raise RuntimeError("standby startup timed out")

    try:
        with tempfile.TemporaryDirectory(prefix="sub2api-app-test-") as temp:
            binary = Path(temp) / "sub2api"
            print("Compiling current backend for local Linux/ARM64", flush=True)
            run(["go", "build", "-o", str(binary), "./cmd/server"], cwd=ROOT / "backend",
                env={**os.environ, "CGO_ENABLED": "0", "GOOS": "linux", "GOARCH": "arm64"}, timeout=600)
            archive = io.BytesIO()
            with tarfile.open(fileobj=archive, mode="w") as tar:
                files = {
                    "Dockerfile": (f"FROM {BASE}\nWORKDIR /app\n"
                                   "COPY sub2api /app/sub2api\n"
                                   "COPY --chown=10001:10001 config.yaml /app/data/config.yaml\n"
                                   "COPY --chown=10001:10001 model_pricing.json /shared/model_pricing.json\n"
                                   "COPY nginx.conf upstream.conf /shared/\n"
                                   "RUN touch /shared/owner.lock /shared/blue.sock.lock /shared/green.sock.lock && "
                                   "chmod 700 /shared && chmod 600 /shared/*.lock && chown -R 10001:10001 /shared\n"
                                   'ENTRYPOINT ["/app/sub2api"]\n').encode(),
                    "config.yaml": json.dumps(config).encode(),
                    "nginx.conf": b"events {}\nhttp { upstream app { include /shared/upstream.conf; } server { listen 8081; location / { proxy_pass http://app; } } }\n",
                    "upstream.conf": b"server blue:8080;\n",
                }
                for name, content in files.items():
                    entry = tarfile.TarInfo(name)
                    entry.size, entry.mode = len(content), 0o600
                    tar.addfile(entry, io.BytesIO(content))
                tar.add(binary, arcname="sub2api")
                tar.add(ROOT / "backend/resources/model-pricing/model_prices_and_context_window.json", arcname="model_pricing.json")
            run(docker + ["build", "--network=none", "-t", image, "-"], data=archive.getvalue(), timeout=120)
        run(docker + ["network", "create", "--internal", network])
        run(docker + ["volume", "create", volume])
        pg = start("postgres", ["-e", "POSTGRES_USER=fixture", "-e", "POSTGRES_DB=fixture",
                                "-e", "POSTGRES_PASSWORD=" + password,
                                "--tmpfs", "/var/lib/postgresql:rw,nosuid,nodev,size=768m"], images["postgres"])
        redis = start("redis", [], images["redis"])
        deadline = time.monotonic() + 45
        while True:
            try:
                if sql("SELECT 1") == "1":
                    break
            except RuntimeError:
                pass
            if time.monotonic() > deadline:
                raise RuntimeError("fixture database startup timed out")
            time.sleep(0.25)
        instance = app("first")
        wait_ready(instance)
        before = snapshot(instance)
        if sql("SELECT count(*) FROM accounts") != "0" or sql("SELECT count(*) FROM users") != "0":
            raise RuntimeError("fixture unexpectedly has business identities")
        print("PASS: isolated application starts with no business accounts", flush=True)
        for dependency in (pg, redis):
            run(docker + ["pause", dependency])
            try:
                time.sleep(1.2)
                tick = time.monotonic()
                if status(instance) != "503" or time.monotonic() - tick >= 4:
                    raise RuntimeError("dependency failure did not produce bounded readiness failure")
                if status(instance, "/health") != "200":
                    raise RuntimeError("dependency failure unexpectedly failed liveness")
            finally:
                run(docker + ["unpause", dependency])
            wait_ready(instance)
        print("PASS: PostgreSQL/Redis pause returns bounded 503 and recovers", flush=True)

        def stop_clean(name):
            run(docker + ["kill", "--signal=TERM", name])
            if run(docker + ["wait", name], timeout=90) != "0":
                raise RuntimeError("application did not exit cleanly")
            logs = run(docker + ["logs", name])
            if "All cleanup steps completed" not in logs or "forced to shutdown" in logs:
                raise RuntimeError("clean shutdown evidence missing")

        stop_clean(instance)
        blue, green = app("blue", True), app("green", True)
        standby(blue)
        standby(green)
        if snapshot(blue) != before:
            raise RuntimeError("standby startup changed migrations, settings, secrets, config or pricing")
        print("PASS: two standby instances share data without bootstrap changes", flush=True)
        state = control(blue, "serve")
        if not state["core_ready"] or status(blue) != "503":
            raise RuntimeError("paused target must be core-ready but not publicly ready")
        control(green, "serve", expected=409)
        control(blue, "resume")
        wait_ready(blue)
        control(blue, "unfreeze")
        for dependency in (pg, redis):
            run(docker + ["pause", dependency])
            try:
                time.sleep(1.2)
                tick = time.monotonic()
                if control(green)["core_ready"] or time.monotonic() - tick >= 4:
                    raise RuntimeError("standby dependency check was not bounded")
                if status(blue) != "503":
                    raise RuntimeError("active dependency failure was hidden")
            finally:
                run(docker + ["unpause", dependency])
            wait_ready(blue)
            standby(green)
        print("PASS: managed active/standby dependency failures are bounded and recover", flush=True)
        proxy = start("nginx", ["--mount", f"type=volume,src={volume},dst=/shared"], BASE,
                      ["nginx", "-g", "daemon off;", "-c", "/shared/nginx.conf"])

        def switch_proxy(target):
            data = io.BytesIO()
            content = ("server " + target.removeprefix(token + "-") + ":8080;\n").encode()
            with tarfile.open(fileobj=data, mode="w") as tar:
                entry = tarfile.TarInfo("upstream.next")
                entry.size, entry.mode = len(content), 0o644
                tar.addfile(entry, io.BytesIO(content))
            run(docker + ["cp", "-", proxy + ":/shared"], data=data.getvalue())
            run(docker + ["exec", proxy, "mv", "/shared/upstream.next", "/shared/upstream.conf"])
            run(docker + ["exec", proxy, "nginx", "-t", "-c", "/shared/nginx.conf"])
            run(docker + ["exec", proxy, "nginx", "-s", "reload", "-c", "/shared/nginx.conf"])
            expected = control(target)["boot_id"]
            deadline = time.monotonic() + 5
            while time.monotonic() < deadline:
                headers = run(docker + ["exec", proxy, "curl", "-q", "-fsS", "--max-time", "2", "-D", "-",
                                       "-o", "/dev/null", "http://127.0.0.1:8081/health"])
                if "x-sub2api-instance: " + expected in headers.lower():
                    return
                time.sleep(0.1)
            raise RuntimeError("Nginx did not route to the selected boot")

        switch_proxy(blue)
        for old, new in ((blue, green), (green, blue)):
            control(old, "freeze")
            control(old, "pause")
            if status(old) != "503":
                raise RuntimeError("old instance still admits traffic")
            state = control(new, "serve")
            if not state["core_ready"] or status(new) != "503":
                raise RuntimeError("paused target must be core-ready but not publicly ready")
            switch_proxy(new)
            if control(new)["phase"] != "serving_paused":
                raise RuntimeError("plugin/tasks restored before traffic")
            control(new, "resume")
            wait_ready(new)
            control(new, "unfreeze")
        print("PASS: exclusive ownership, Nginx reload, traffic-before-resume and old/new/old handoff", flush=True)
        boot = control(blue)["boot_id"]
        run(docker + ["restart", "-t", "90", blue], timeout=100)
        standby(blue)
        if control(blue)["boot_id"] == boot:
            raise RuntimeError("restarted process reused boot identity")
        control(blue, "serve")
        switch_proxy(blue)
        control(blue, "resume")
        wait_ready(blue)
        print("PASS: restart defaults to standby and can resume the same selected slot", flush=True)
        control(blue, "pause")
        stop_clean(blue)
        stop_clean(green)
        print(f"Local shared-database checks passed in {time.monotonic()-started:.1f}s. No signed plugin, billing or production ingress acceptance.")
    except Exception:
        for name in containers:
            result = subprocess.run(docker + ["logs", "--tail=35", name], capture_output=True, timeout=10)
            print(name + ":\n" + (result.stdout + result.stderr).decode(errors="replace"), flush=True)
        raise
    finally:
        failures = []
        for kind, name in [("container", n) for n in containers] + [("volume", volume), ("network", network), ("image", image)]:
            exists = subprocess.run(docker + [kind, "inspect", name], capture_output=True, timeout=15)
            if exists.returncode:
                error = (exists.stdout + exists.stderr).decode(errors="replace").lower()
                if "no such" not in error and not (kind == "network" and "network " + name + " not found" in error):
                    failures.append(name)
                continue
            args = [kind, "rm", *(["-f", "-v"] if kind == "container" else []), name]
            result = subprocess.run(docker + args, capture_output=True, timeout=30)
            if result.returncode:
                failures.append(name)
        if failures:
            raise RuntimeError("Local cleanup failed; retain for inspection: " + ", ".join(failures))


if __name__ == "__main__":
    main()

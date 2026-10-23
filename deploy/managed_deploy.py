#!/usr/bin/env python3
"""Private Unix-socket controller for reviewed, application-only deployments."""
import argparse
from contextlib import contextmanager
import errno
import fcntl
from http.server import BaseHTTPRequestHandler
import json
import os
from pathlib import Path
import re
import shutil
import socket
import socketserver
import stat
import time
import uuid
from urllib.request import urlopen
from urllib.parse import urlsplit, parse_qs

import build_fast_release as build
import fast_deploy as d


ID = re.compile(r"[a-z0-9][a-z0-9-]{0,63}")
SHA = re.compile(r"[0-9a-f]{40}")
VERSION = re.compile(r"v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\+r17\.[1-9][0-9]*")
CATALOG_URL = "https://raw.githubusercontent.com/jzg-lab/sub2api-r3-fix/main/.github/deployment-candidates.json"
PREPARING = {"QUEUED", "FETCHING", "BUILDING", "VERIFYING", "BACKUP"}
FINISHED = d.TERMINAL | {"PREPARATION_FAILED", "BLOCKED", "PREPARED", "EXPIRED"}


class PreparationBlocked(RuntimeError):
    pass


def admission(condition, reason):
    if not condition:
        raise PreparationBlocked(reason)


def private_file(path):
    path = Path(path)
    d.require(path.is_absolute() and path.is_file() and not path.is_symlink(), "private file required")
    info = path.stat()
    d.require(info.st_uid == os.geteuid() and info.st_mode & 0o077 == 0, "private file owner or permissions differ")
    return path


class Controller:
    def __init__(self, config_path):
        self.config_path = private_file(config_path)
        self.c = d.read(self.config_path)
        self.root = Path(self.c["state_dir"])
        d.require(re.fullmatch(r"/[A-Za-z0-9_./-]+", str(self.root)), "invalid state directory")
        d.require(self.root.is_dir() and not self.root.is_symlink() and self.root.stat().st_uid == os.geteuid()
                  and self.root.stat().st_mode & 0o077 == 0, "private state directory required")
        d.require(re.fullmatch(r"/[A-Za-z0-9_./-]+", str(self.config_path)), "invalid config path")
        # Missing mode belongs to the historical single-instance installation.
        self.mode = self.c.get("mode", "single")
        d.require(self.mode in ("single", "handoff"), "unsupported deployment mode")
        self.config_digest = d.digest(self.config_path)
        fd = os.open(self.root / "control.lock", os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
        os.close(fd)

    @contextmanager
    def lock(self):
        path = self.root / "control.lock"
        before = path.lstat()
        d.require(stat.S_ISREG(before.st_mode) and before.st_mode & 0o077 == 0, "unsafe controller lock")
        fd = os.open(path, os.O_RDWR | os.O_NOFOLLOW)
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
            d.require(os.path.samestat(before, os.fstat(fd)) and os.path.samestat(before, path.lstat()),
                      "controller lock replaced")
            yield
        finally:
            os.close(fd)

    def launch_prepare(self, job):
        script = Path(__file__).resolve()
        d.require(re.fullmatch(r"/[A-Za-z0-9_./-]+", str(script)), "invalid controller path")
        d.run("systemd-run", "--unit=sub2api-prepare-" + job.name,
              "--property=Type=exec", "--property=UMask=0077",
              "--property=StandardOutput=append:" + str(job / "task.log"),
              "--property=StandardError=append:" + str(job / "task.log"),
              "--property=ExecStopPost=/usr/bin/python3 " + str(script) + " interrupted --config " + str(self.config_path) + " --job " + job.name,
              "/usr/bin/python3", str(script), "prepare", "--config", str(self.config_path), "--job", job.name)

    def catalog(self, refresh=False):
        path = Path(self.c["candidates_file"])
        if self.c.get("github_catalog") and (refresh or not path.exists() or time.time() - path.stat().st_mtime > 300):
            with urlopen(CATALOG_URL, timeout=5) as response:
                d.require(response.geturl() == CATALOG_URL, "unexpected candidate registry redirect")
                data = response.read(131073)
            d.require(len(data) <= 131072, "candidate registry too large")
            candidates = self.validate_catalog(json.loads(data))
            d.save(path, candidates)
        return self.validate_catalog(d.read(private_file(path)))

    @staticmethod
    def validate_catalog(candidates):
        d.require(isinstance(candidates, list) and len(candidates) <= 100, "invalid catalog")
        seen = set()
        sequences = set()
        for item in candidates:
            d.require(ID.fullmatch(item["id"]) and item["id"] not in seen, "invalid candidate id")
            seen.add(item["id"])
            d.require(SHA.fullmatch(item["source"]) and SHA.fullmatch(item["previous_source"]), "full commits required")
            d.require(VERSION.fullmatch(item["version"]), "project release version required")
            sequence = int(item["version"].rsplit(".", 1)[1])
            d.require(sequence not in sequences, "release sequence reused")
            sequences.add(sequence)
            d.require(item["channel"] in ("ordinary", "special") and isinstance(item["summary"], str), "invalid candidate")
            d.require(isinstance(item.get("review"), str) and item["review"].strip(), "compatibility review required")
        return candidates

    def latest(self, pointer="latest.json"):
        path = self.root / pointer
        if not path.exists():
            return None
        job_id = d.read(path)["id"]
        d.require(ID.fullmatch(job_id), "invalid saved job id")
        return self.root / job_id

    def rollback_job(self):
        latest = self.latest()
        state = self.job_status(latest) if latest else None
        if state and state["phase"] not in FINISHED:
            return None
        accepted = latest if state and state["phase"] == "ACCEPTED" else self.latest("accepted.json")
        result = self.job_status(accepted) if accepted else None
        return result if result and "rollback" in result["actions"] else None

    def job_status(self, job):
        state = d.read(job / "state.json")
        mode = state.get("mode", "single")
        d.require(mode in ("single", "handoff"), "unsupported saved deployment mode")
        phase = state["phase"]
        release = job / "release"
        attempt = state.get("attempt")
        if attempt:
            d.require(attempt in ("activate", "rollback"), "invalid saved attempt")
            path = release / attempt / "state.json"
            if path.exists():
                phase = d.read(path)["phase"]
        expires = 0
        previous_version = ""
        auxiliary_pending = False
        retiring = False
        live_handoff = False
        if (release / "prepared.json").exists():
            prepared, config = d.read(release / "prepared.json"), d.read(release / "config.json")
            live_handoff = prepared.get("handoff", {}).get("protocol") == 2
            if mode == "handoff":
                active = Path(config["deploy_dir"]) / "handoff-active.json"
                auxiliary_pending = active.exists() and bool(d.read(active).get("pending_services"))
                retiring = active.exists() and bool(d.read(active).get("retiring"))
            previous = prepared.get("previous", {})
            identity = re.search(r"\bSub2API (v?[0-9][A-Za-z0-9.+-]*) \(commit: ([0-9a-f]{40}), built: [^\r\n)]*\)", previous.get("version", ""))
            if identity and identity[2] == previous.get("source") == state.get("previous_source"):
                previous_version = "v" + identity[1].removeprefix("v")
            expires = prepared["backup_at"] + config["backup_max_age_seconds"]
            valid = prepared["backup_at"] <= time.time() <= expires
            if phase == "PREPARED" and not valid:
                phase = "EXPIRED"
        else:
            valid = False
        actions = []
        if valid and phase == "PREPARED" and not (release / "activate").exists():
            actions = ["activate"]
        if valid and phase == "ACCEPTED" and not (release / "rollback").exists():
            actions = ["rollback"]
        error_code = state.get("error_code", "")
        if auxiliary_pending:
            actions = []
            error_code = error_code or "AUXILIARY_SERVICES_PENDING"
        if retiring:
            actions = []
            error_code = error_code or "PREVIOUS_REQUESTS_PENDING"
        return {k: state[k] for k in ("id", "candidate_id", "version", "source")} | {
            "mode": mode, "phase": phase, "expires_at": expires, "actions": actions,
            "previous_source": state.get("previous_source", ""),
            "previous_version": previous_version,
            "live_handoff": live_handoff,
            "reused": state.get("reused", False),
            "error_code": error_code or ("CHECK_FAILED" if phase in ("PREPARATION_FAILED", "PREFLIGHT_FAILED", "LAUNCH_FAILED", "NEEDS_ATTENTION") else ""),
        }

    def snapshot(self, refresh=False):
        catalog_error = ""
        try:
            candidates = [{k: item.get(k, "") for k in ("id", "version", "source", "summary", "channel", "reason")}
                          for item in self.catalog(refresh)]
        except Exception:
            candidates, catalog_error = [], "CATALOG_UNAVAILABLE"
        latest = self.latest()
        job = self.job_status(latest) if latest else None
        if catalog_error and job:
            job["actions"] = [action for action in job["actions"] if action == "rollback"]
        return {"enabled": True, "candidates": candidates, "job": job,
                "rollback_job": self.rollback_job(), "catalog_error": catalog_error}

    def command(self, action, identifier, *, special_review=None):
        d.require(isinstance(identifier, str) and ID.fullmatch(identifier), "invalid identifier")
        d.require(action in ("prepare", "activate", "rollback"), "invalid action")
        review = None
        if special_review is not None:
            d.require(action == "prepare", "special review is preparation only")
            review = d.read(private_file(special_review))
            d.require(isinstance(review, dict), "invalid special review")
        with self.lock():
            d.require(d.digest(self.config_path) == self.config_digest, "controller configuration changed; restart required")
            config = d.read(private_file(self.c["deployment_config"]))
            d.require(config.get("mode", "single") == self.mode, "deployment configuration mode differs")
            host = d.deployment_host(config)
            latest = self.latest()
            state = self.job_status(latest) if latest else None
            d.require(not state or state["mode"] == self.mode, "deployment mode changed; explicit installation required")
            if action == "prepare":
                d.require(not state or state.get("error_code") not in
                          ("AUXILIARY_SERVICES_PENDING", "PREVIOUS_REQUESTS_PENDING"), "previous handoff still finishing")
                # Repeated clicks attach to the same unfinished preparation, never duplicate it.
                if state and state["candidate_id"] == identifier and state["phase"] in PREPARING | {"PREPARED"}:
                    return self.snapshot()
                d.require(not state or state["phase"] in FINISHED, "another operation requires attention")
                d.require(not host.pending.exists() and not host.marker.exists(), "deployment needs recovery")
                candidate = next((c for c in self.catalog(refresh=True) if c["id"] == identifier), None)
                d.require(candidate, "special or unknown candidate")
                if review is None:
                    d.require(candidate["channel"] == "ordinary", "special or unknown candidate")
                else:
                    d.require(candidate["channel"] == "special" and review.get("status") == "approved" and
                              review.get("source") == candidate["source"] and
                              review.get("previous_source") == candidate["previous_source"] and
                              isinstance(review.get("changed_paths"), list) and
                              all(isinstance(review.get(k), str) and review[k].strip()
                                  for k in ("reviewed_by", "evidence")), "special review differs from candidate")
                if state and state["phase"] == "ACCEPTED":
                    d.save(self.root / "accepted.json", {"id": state["id"]})
                job = self.root / ("job-" + uuid.uuid4().hex)
                job.mkdir(mode=0o700)
                d.save(job / "candidate.json", candidate)
                if review is not None:
                    d.save(job / "special-review.json", review)
                d.save(job / "state.json", {"id": job.name, "candidate_id": identifier, "version": candidate["version"],
                                           "source": candidate["source"], "previous_source": candidate["previous_source"],
                                           "mode": self.mode, "phase": "QUEUED", "created_at": time.time()})
                d.save(self.root / "latest.json", {"id": job.name})
                try:
                    self.launch_prepare(job)
                except Exception:
                    self.state(job, "NEEDS_ATTENTION")
                    raise
            else:
                rollback = self.rollback_job() if action == "rollback" else None
                if rollback and rollback["id"] == identifier:
                    latest, state = self.root / identifier, rollback
                d.require(latest and state["id"] == identifier, "job is no longer current")
                release = latest / "release"
                if (release / action).exists():
                    return self.snapshot()
                d.require(action in state["actions"], "action unavailable or expired")
                if action == "activate":
                    d.require(d.read(latest / "candidate.json") in self.catalog(refresh=True), "candidate approval changed")
                else:
                    d.save(self.root / "accepted.json", {"id": latest.name})
                    d.save(self.root / "latest.json", {"id": latest.name})
                self.state(latest, "SWITCHING", attempt=action)
                try:
                    # Invoke the sealed runner, not scripts from the selected candidate.
                    d.run("/usr/bin/python3", str(release / "tools/fast_deploy.py"), action, "--release", str(release), timeout=30)
                except Exception:
                    self.state(latest, "NEEDS_ATTENTION", attempt=action)
                    raise
        return self.snapshot()

    def state(self, job, phase, **fields):
        state = d.read(job / "state.json")
        state.update(phase=phase, updated_at=time.time(), **fields)
        d.save(job / "state.json", state)

    def verification_context(self, job, host):
        from verify_fast_release import approved_inputs
        inputs, plugins = approved_inputs(self.c, host)
        tools = Path(__file__).resolve().parent
        return {"candidate": d.read(job / "candidate.json"), "controller": self.c,
                "special_review": d.digest(job / "special-review.json") if (job / "special-review.json").exists() else None,
                "deployment_config": d.digest(self.c["deployment_config"]), "inputs": inputs,
                "tools": {name: d.digest(tools / name) for name in (
                    "managed_deploy.py", "verify_fast_release.py", "build_fast_release.py",
                    "fast_deploy.py", "handoff_deploy.py", "FastRuntime.Dockerfile")},
                "app": host.app_facts(), "data": host.data_identity(), "migrations": host.migrations(),
                "plugins": plugins, "files": host.config_hashes(), "compose": host.compose_hash(),
                "env": d.digest(host.c["env_file"])}

    def reuse_verified(self, job, context, host):
        previous = self.latest("verified.json")
        if not previous:
            return False
        try:
            seal = d.read(previous / "verified.json")
            d.require(seal["context"] == context, "verification context changed")
            artifact = d.read(previous / "build/artifact.json")
            d.evidence(previous / "release", artifact, host)
            d.require(host.inspect(artifact["image"], image=True)["Id"] == artifact["image_id"], "verified image changed")
            files = self.verified_files(previous)
            d.require({name: d.digest(previous / name) for name in files} == seal["files"], "verified materials changed")
        except (OSError, RuntimeError, ValueError, KeyError, TypeError):
            return False
        # Copy only immutable verification material, never an old backup or attempt.
        for name in files:
            target = job / name
            target.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
            d.atomic(target, (previous / name).read_bytes())
        self.state(job, "VERIFYING", reused=True, reused_from=previous.name)
        return True

    @staticmethod
    def verified_files(job):
        receipt = d.read(job / "release/evidence.json")
        for check in receipt["checks"].values():
            path = (job / "release" / check["log"]).resolve()
            d.require((job / "release").resolve() in path.parents, "invalid verification log path")
        return ["build/artifact.json", "release/config.json", "release/evidence.json",
                *("release/" + check["log"] for check in receipt["checks"].values())]

    def seal_verification(self, job, context, host):
        d.require(self.verification_context(job, host) == context, "verification context changed during rehearsal")
        d.require(d.read(job / "lab/cleanup.json")["status"] == "passed", "fixture cleanup incomplete")
        d.evidence(job / "release", d.read(job / "build/artifact.json"), host)
        files = self.verified_files(job)
        d.save(job / "verified.json", {"context": context, "files": {name: d.digest(job / name) for name in files}})
        d.save(self.root / "verified.json", {"id": job.name})

    def prepare(self, job_id):
        d.require(ID.fullmatch(job_id), "invalid job id")
        job = self.root / job_id
        d.ERROR_LOG = job / "command-errors.log"
        with (job / "worker.lock").open("a") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            d.require(d.read(job / "state.json")["phase"] == "QUEUED", "preparation cannot be replayed")
            try:
                with self.lock():
                    d.require(self.latest() == job and d.read(job / "state.json").get("mode", "single") == self.mode,
                              "preparation no longer owns the deployment")
                self.state(job, "FETCHING")
                candidate = d.read(job / "candidate.json")
                admission(candidate in self.catalog(), "CANDIDATE_CHANGED")
                config = d.read(private_file(self.c["deployment_config"]))
                d.require(config.get("mode", "single") == self.mode, "deployment mode changed")
                host = d.deployment_host(config)
                deployment_config = Path(self.c["deployment_config"])
                d.require(not host.marker.exists() and not host.pending.exists(), "deployment needs recovery")
                old = host.app_facts()
                admission(candidate["previous_source"] in old["version"], "BASELINE_CHANGED")
                admission(shutil.disk_usage(self.root).free >= self.c["min_free_bytes"], "INSUFFICIENT_RESOURCES")
                memory = dict(line.split(":", 1) for line in Path("/proc/meminfo").read_text().splitlines())
                admission(int(memory["MemAvailable"].split()[0]) * 1024 >= self.c["min_available_memory_bytes"], "INSUFFICIENT_RESOURCES")
                repo = self.c["repo"]
                d.require(build.git(repo, "remote", "get-url", "origin") == "https://github.com/jzg-lab/sub2api-r3-fix.git", "unexpected repository")
                d.run("git", "-C", repo, "fetch", "--no-tags", "origin", candidate["source"], timeout=180)
                changed = build.git(repo, "diff", "--name-only", candidate["previous_source"], candidate["source"]).splitlines()
                special = job / "special-review.json"
                if candidate["channel"] == "special":
                    review = d.read(private_file(special))
                    build.check_contract(repo, candidate["previous_source"], candidate["source"], review)
                else:
                    admission(not special.exists() and not any(build.protected(p) or p.startswith(
                        ("deploy/", "backend/internal/config/")) for p in changed), "SPECIAL_DEPLOYMENT_REQUIRED")
                version = build.git(repo, "show", candidate["source"] + ":backend/cmd/server/VERSION").removeprefix("v")
                admission("v" + version == candidate["version"], "VERSION_MISMATCH")
                context = self.verification_context(job, host)
                release = job / "release"
                if not self.reuse_verified(job, context, host):
                    try:
                        build.limited_build_options(build.BUILD_CGROUP)
                    except (OSError, RuntimeError, ValueError, KeyError):
                        raise PreparationBlocked("BUILD_BUDGET_UNAVAILABLE") from None
                    self.state(job, "BUILDING")
                    build.build(argparse.Namespace(repo=repo, output=str(job / "build"), commit=candidate["source"],
                                previous=candidate["previous_source"], runtime=old["image_id"],
                                node_image=self.c["node_image"], go_image=self.c["go_image"], cgroup_parent=build.BUILD_CGROUP,
                                special_review=str(special) if candidate["channel"] == "special" else None))
                    self.state(job, "VERIFYING")
                    from verify_fast_release import verify
                    release.mkdir(mode=0o700)
                    verify(job, {**self.c, "deployment_config": str(deployment_config)}, host)
                    self.seal_verification(job, context, host)
                self.state(job, "BACKUP")
                d.prepare(release, deployment_config, job / "build/artifact.json")
                self.state(job, "PREPARED")
            except PreparationBlocked as error:
                self.state(job, "BLOCKED", error_code=str(error))
                raise
            except Exception:
                cleanup = job / "lab/cleanup.json"
                phase = d.read(job / "state.json")["phase"]
                self.state(job, "NEEDS_ATTENTION" if cleanup.exists() and d.read(cleanup)["status"] != "passed" else "PREPARATION_FAILED",
                           error_code=phase + "_FAILED")
                raise

    def interrupted(self, job_id):
        d.require(isinstance(job_id, str) and ID.fullmatch(job_id), "invalid job id")
        job = self.root / job_id
        with self.lock():
            if d.read(job / "state.json")["phase"] in PREPARING:
                self.state(job, "NEEDS_ATTENTION")


class Handler(BaseHTTPRequestHandler):
    def setup(self):
        self.request.settimeout(15)
        super().setup()

    def log_message(self, *_):
        pass

    def handle_request(self):
        try:
            self.connection.settimeout(15)
            control = self.server.control
            url = urlsplit(self.path)
            if self.command == "GET" and url.path == "/status":
                result = control.snapshot(parse_qs(url.query).get("refresh") == ["true"])
            else:
                d.require(self.command == "POST" and self.path in ("/prepare", "/activate", "/rollback"), "invalid action")
                length = int(self.headers.get("Content-Length", "0"))
                d.require(0 < length <= 1024 and not self.headers.get("Transfer-Encoding"), "invalid request size")
                body = json.loads(self.rfile.read(length))
                d.require(isinstance(body, dict) and set(body) == {"id"}, "invalid request fields")
                result = control.command(self.path[1:], body["id"])
            body = json.dumps(result).encode()
            code = 200
        except (RuntimeError, BlockingIOError, ValueError, KeyError, TypeError):
            body, code = b'{"error":"DEPLOYMENT_CONFLICT"}', 409
        except Exception:
            body, code = b'{"error":"EXECUTOR_UNAVAILABLE"}', 503
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    do_GET = handle_request
    do_POST = handle_request


@contextmanager
def controller_server(control):
    path = Path(control.c["socket_path"])
    parent = path.parent
    d.require(path.is_absolute() and parent.is_dir() and not parent.is_symlink(), "stable socket directory required")
    info = parent.stat()
    d.require(info.st_uid == os.geteuid() and not info.st_mode & 0o022, "unsafe socket directory")
    with (control.root / "server.lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        if path.exists() or path.is_symlink():
            info = path.lstat()
            d.require(stat.S_ISSOCK(info.st_mode) and info.st_uid == os.geteuid(), "unexpected socket path")
            with socket.socket(socket.AF_UNIX) as probe:
                probe.settimeout(1)
                try:
                    probe.connect(str(path))
                except OSError as error:
                    d.require(error.errno == errno.ECONNREFUSED, "socket state unknown")
                else:
                    raise RuntimeError("controller already listening")
            path.unlink()
        with socketserver.UnixStreamServer(str(path), Handler) as server:
            identity = path.stat().st_ino
            try:
                os.chown(path, os.geteuid(), control.c["socket_gid"])
                path.chmod(0o660)
                server.control = control
                yield server
            finally:
                if path.exists() and path.lstat().st_ino == identity:
                    path.unlink()


def main():
    os.umask(0o077)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("serve", "prepare", "prepare-special", "interrupted", "reconcile"))
    parser.add_argument("--config", required=True)
    parser.add_argument("--job")
    parser.add_argument("--candidate")
    parser.add_argument("--special-review")
    args = parser.parse_args()
    d.require(os.geteuid() == 0, "executor must be installed by the host administrator")
    control = Controller(args.config)
    if args.command == "prepare":
        control.prepare(args.job)
    elif args.command == "prepare-special":
        d.require(args.special_review, "explicit special review required")
        print(json.dumps(control.command("prepare", args.candidate, special_review=args.special_review)))
    elif args.command == "interrupted":
        control.interrupted(args.job)
    elif args.command == "reconcile":
        d.require(control.mode == "handoff", "reconciliation requires handoff installation")
        from handoff_deploy import reconcile
        reconcile(control.c)
    else:
        with controller_server(control) as server:
            server.serve_forever()


if __name__ == "__main__":
    main()

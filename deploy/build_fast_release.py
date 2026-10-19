#!/usr/bin/env python3
"""Build a native, application-only image from a fixed Git commit. Never deploy."""
import argparse
import hashlib
import json
from pathlib import Path
import re
import subprocess
import time

import fast_deploy
from fast_deploy import digest, require, run, save


def git(repo, *args):
    return run("git", "-C", str(repo), *args).decode().strip()


def protected(path):
    # Fail closed on dependencies/protocol/runtime changes; these need review.
    return (path in ("Dockerfile", "backend/go.mod", "backend/go.sum") or
            path.startswith(("backend/migrations/", "backend/pkg/pluginapi/",
                             "backend/internal/repository/migrations_runner",
                             "deploy/codex-lb-cookie-pin/", "deploy/auth-browser/",
                             "deploy/docker-entrypoint", "deploy/client-identity/")))


def check_contract(repo, previous, commit):
    changed = git(repo, "diff", "--name-only", previous, commit).splitlines()
    blocked = [p for p in changed if protected(p)]
    require(not blocked, "special deployment required: " + ", ".join(blocked))


def deployment_version(source_version, commit):
    version = source_version.strip().removeprefix("v")
    require(re.fullmatch(r"(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)"
                         r"(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?"
                         r"(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?", version), "invalid source version")
    # Keep the release ordering used by plugin compatibility checks.
    return version + ("." if "+" in version else "+") + "fast." + commit[:12]


def build(args):
    repo, output = Path(args.repo).resolve(), Path(args.output).resolve()
    for ref in (args.commit, args.previous):
        require(re.fullmatch(r"[0-9a-f]{40}", ref), "full Git commit required")
        require(git(repo, "rev-parse", ref + "^{commit}") == ref, "commit not found")
    check_contract(repo, args.previous, args.commit)
    require(repo not in output.parents, "build artifacts belong outside the repository")
    for image in (args.node_image, args.go_image):
        require(re.fullmatch(r"[^\s]+@sha256:[0-9a-f]{64}", image), "pin build images by digest")
    base = json.loads(run("docker", "image", "inspect", args.runtime))[0]
    require(base["Os"] == "linux" and base["Architecture"] in ("amd64", "arm64"), "unsupported runtime")
    arch = base["Architecture"]
    require(run("docker", "info", "--format", "{{.Architecture}}").strip().decode() in
            ({"amd64": ("amd64", "x86_64"), "arm64": ("arm64", "aarch64")}[arch]), "native builds only")
    output.mkdir(mode=0o700, parents=False, exist_ok=False)
    fast_deploy.ERROR_LOG = output / "command-errors.log"
    source = output / "source"
    source.mkdir()
    run("git", "-C", str(repo), "archive", "--output=" + str(output / "source.tar"), args.commit)
    run("tar", "-xf", str(output / "source.tar"), "-C", str(source))
    image = "sub2api:fast-" + args.commit + "-" + str(time.time_ns())
    builder = image + "-build"
    runtime = image + "-base"
    run("docker", "tag", base["Id"], runtime)
    version = deployment_version((source / "backend/cmd/server/VERSION").read_text(), args.commit)
    with (output / "build.log").open("wb") as log:
        run("docker", "build", "--progress=plain", "--target", "backend-builder",
            "--build-arg", "NODE_IMAGE=" + args.node_image, "--build-arg", "GOLANG_IMAGE=" + args.go_image,
            "--build-arg", "TARGETOS=linux", "--build-arg", "TARGETARCH=" + arch,
            "--build-arg", "VERSION=" + version, "--build-arg", "COMMIT=" + args.commit,
            "--build-arg", "DATE=" + time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
            "-t", builder, str(source), timeout=3600, stdout=log, merge_stderr=True)
        builder_id = json.loads(run("docker", "image", "inspect", builder))[0]["Id"]
        run("docker", "build", "--progress=plain", "-f", str(Path(__file__).with_name("FastRuntime.Dockerfile")),
            "--build-arg", "BUILDER=" + builder, "--build-arg", "RUNTIME=" + runtime,
            "--build-arg", "COMMIT=" + args.commit, "--build-arg", "VERSION=" + version,
            "-t", image, str(source), timeout=1800, stdout=log, merge_stderr=True)
    candidate = json.loads(run("docker", "image", "inspect", image))[0]
    require(json.loads(run("docker", "image", "inspect", builder))[0]["Id"] == builder_id and
            json.loads(run("docker", "image", "inspect", runtime))[0]["Id"] == base["Id"], "build input tag changed")
    require(candidate["RootFS"]["Layers"][:len(base["RootFS"]["Layers"])] == base["RootFS"]["Layers"],
            "runtime layers changed")
    for key in ("Env", "User", "WorkingDir", "Entrypoint", "Cmd", "Healthcheck"):
        require(candidate["Config"].get(key) == base["Config"].get(key), "runtime config changed: " + key)
    version_output = run("docker", "run", "--rm", "--network", "none", "--entrypoint", "/app/sub2api", image, "-version")
    require(args.commit.encode() in version_output, "binary commit mismatch")
    binary = run("docker", "run", "--rm", "--network", "none", "--entrypoint", "sha256sum", image, "/app/sub2api").decode().split()[0]
    container = run("docker", "create", builder).decode().strip()
    try:
        run("docker", "cp", container + ":/app/backend/internal/web/dist", str(output / "frontend"))
    finally:
        run("docker", "rm", container)
    assets = {"/" + p.relative_to(output / "frontend").as_posix(): digest(p)
              for p in (output / "frontend/assets").rglob("*") if p.is_file()}
    require(assets, "missing embedded assets")
    migrations = {p.name: hashlib.sha256(p.read_bytes().strip()).hexdigest()
                  for p in (source / "backend/migrations").glob("*.sql")}
    save(output / "artifact.json", {
        "format": 1, "source": args.commit, "previous_source": args.previous,
        "image": image, "image_id": candidate["Id"], "base_image_id": base["Id"],
        "architecture": arch, "binary_sha256": binary, "assets": assets, "migrations": migrations,
        "builder_id": builder_id, "node_image": args.node_image, "go_image": args.go_image,
        "source_archive_sha256": digest(output / "source.tar"),
        "build_log_sha256": digest(output / "build.log"), "runtime_dockerfile_sha256": digest(Path(__file__).with_name("FastRuntime.Dockerfile")),
    })
    print(str(output / "artifact.json"))


if __name__ == "__main__":
    import os
    os.umask(0o077)
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("repo", "output", "commit", "previous", "runtime", "node-image", "go-image"):
        parser.add_argument("--" + name, required=True)
    build(parser.parse_args())

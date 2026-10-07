"""Validate auth-browser proxy, exit-IP, and authorization URL inputs."""

import hashlib
import ipaddress
import os
from pathlib import Path
import re
import shutil
import stat
import sys
import time
from urllib.parse import parse_qs, urlsplit


STATIC_AUTH_BROWSER_PORTS = frozenset(range(17931, 17935))
AUTH_PROFILE_PATTERN = re.compile(r"auth-[0-9a-f]{64}")


def normalize_proxy(value):
    value = value.strip()
    if not value or any(character.isspace() or ord(character) < 32 for character in value):
        raise ValueError("代理地址不能为空，也不能包含空格或换行。")
    if "://" not in value:
        value = "http://" + value
    try:
        parsed = urlsplit(value)
        host, port = parsed.hostname, parsed.port
    except ValueError:
        raise ValueError("代理地址格式不正确。IPv6 地址请使用 [地址]:端口。") from None
    scheme = parsed.scheme.lower()
    if scheme == "socks5h":
        scheme = "socks5"
    if scheme not in ("http", "https", "socks5"):
        raise ValueError("只支持 HTTP、HTTPS 和 SOCKS5 代理。")
    if parsed.username is not None or parsed.password is not None:
        raise ValueError("授权浏览器只接受免认证代理入口，不能包含账号密码。")
    if parsed.path not in ("", "/") or parsed.query or parsed.fragment or "?" in value or "#" in value:
        raise ValueError("这里需要代理 IP/主机和端口，不是网页链接。")
    if not host or port is None or not 1 <= port <= 65535:
        raise ValueError("请填写代理地址和端口，例如 192.0.2.10:8080；端口范围为 1-65535。")
    try:
        address = ipaddress.ip_address(host)
    except ValueError:
        labels = host.split(".")
        if (
            len(host) > 253
            or re.fullmatch(r"[0-9.]+", host)
            or any(
                not re.fullmatch(
                    r"[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?", label
                )
                for label in labels
            )
        ):
            raise ValueError("代理 IP 或主机名格式不正确。") from None
    else:
        if "%" in host:
            raise ValueError("暂不支持带网络接口标识的 IPv6 代理地址。")
        host = "[{}]".format(address) if address.version == 6 else str(address)
    return "{}://{}:{}".format(scheme, host, port)


def normalize_exit_ip(value):
    try:
        return str(ipaddress.ip_address(value.strip()))
    except ValueError:
        raise ValueError("代理没有返回有效的出口 IP。") from None


def validate_exit_ip(value, proxy=None, environ=None):
    actual = normalize_exit_ip(value)
    if proxy is None:
        return actual

    normalized_proxy = normalize_proxy(proxy)
    parsed_proxy = urlsplit(normalized_proxy)
    environment = os.environ if environ is None else environ
    expected_key = "SUB2API_AUTH_BROWSER_EXPECTED_EXIT_{}".format(parsed_proxy.port)
    expected_raw = environment.get(expected_key, "").strip()
    require_static = environment.get(
        "SUB2API_AUTH_BROWSER_REQUIRE_STATIC_EXIT_CHECKS", ""
    ).strip().lower() in ("1", "true", "yes")
    is_static_ingress = (
        parsed_proxy.hostname in ("127.0.0.1", "::1", "localhost")
        and parsed_proxy.port in STATIC_AUTH_BROWSER_PORTS
    )

    if require_static and is_static_ingress and not expected_raw:
        raise ValueError("静态 ISP 授权入口缺少预期出口 IP，拒绝启动浏览器。")
    if expected_raw:
        expected = normalize_exit_ip(expected_raw)
        if actual != expected:
            raise ValueError("授权代理出口 IP 与绑定桶不一致，拒绝启动浏览器。")
    return actual


def profile_lock_is_active(lock_path):
    if not lock_path.is_symlink():
        return lock_path.exists()
    try:
        target = os.readlink(str(lock_path))
    except OSError:
        return True
    pid_text = target.rsplit("-", 1)[-1]
    if not pid_text.isdigit():
        return True
    try:
        os.kill(int(pid_text), 0)
    except ProcessLookupError:
        return False
    except PermissionError:
        return True
    return True


def prune_stale_profiles(root_value, current_name, max_age_hours, now=None):
    if not AUTH_PROFILE_PATTERN.fullmatch(current_name):
        raise ValueError("授权浏览器 profile 标识不合法。")
    try:
        max_age_hours = int(max_age_hours)
    except (TypeError, ValueError):
        raise ValueError("授权浏览器 profile 保留时长不合法。") from None
    if not 1 <= max_age_hours <= 8760:
        raise ValueError("授权浏览器 profile 保留时长必须在 1-8760 小时之间。")

    root = Path(root_value)
    if root.is_symlink():
        raise ValueError("授权浏览器 profile 根目录不能是符号链接。")
    if not root.exists():
        return 0
    if not root.is_dir():
        raise ValueError("授权浏览器 profile 根路径不是目录。")

    cutoff = (time.time() if now is None else now) - max_age_hours * 3600
    removed = 0
    for entry in root.iterdir():
        if (
            entry.name == current_name
            or not AUTH_PROFILE_PATTERN.fullmatch(entry.name)
            or entry.is_symlink()
            or not entry.is_dir()
        ):
            continue
        lock_path = entry / "SingletonLock"
        if (lock_path.exists() or lock_path.is_symlink()) and profile_lock_is_active(lock_path):
            continue
        try:
            modified = entry.lstat().st_mtime
        except OSError:
            continue
        if modified >= cutoff:
            continue
        shutil.rmtree(str(entry))
        removed += 1
    return removed


def normalize_auth_url(value):
    value = value.strip()
    if not value or any(ord(character) < 32 for character in value):
        raise ValueError("授权链接不能为空，也不能包含控制字符。")
    try:
        parsed = urlsplit(value)
        port = parsed.port
    except ValueError:
        raise ValueError("授权链接格式不正确。") from None
    if (
        parsed.scheme.lower() != "https"
        or parsed.hostname != "auth.openai.com"
        or port not in (None, 443)
        or parsed.username is not None
        or parsed.password is not None
        or parsed.path != "/oauth/authorize"
        or not parsed.query
        or parsed.fragment
    ):
        raise ValueError("只允许 OpenAI 官方 HTTPS 授权链接。")

    try:
        params = parse_qs(parsed.query, keep_blank_values=True, strict_parsing=True)
    except ValueError:
        raise ValueError("OpenAI 授权链接的查询参数格式不正确。") from None

    def required_single(name):
        values = params.get(name, [])
        if len(values) != 1 or not values[0]:
            raise ValueError("OpenAI 授权链接缺少或重复必要参数。")
        return values[0]

    state = required_single("state")
    if not re.fullmatch(r"[0-9a-f]{64}", state):
        raise ValueError("OpenAI 授权链接的 state 不合法。")
    if required_single("response_type") != "code":
        raise ValueError("OpenAI 授权链接的 response_type 不合法。")
    if required_single("code_challenge_method") != "S256":
        raise ValueError("OpenAI 授权链接的 PKCE 方法不合法。")
    if not re.fullmatch(r"[A-Za-z0-9_-]{43}", required_single("code_challenge")):
        raise ValueError("OpenAI 授权链接的 PKCE challenge 不合法。")
    required_single("client_id")
    required_single("redirect_uri")
    return value


def auth_profile_tag(value):
    normalized = normalize_auth_url(value)
    state = parse_qs(urlsplit(normalized).query, strict_parsing=True)["state"][0]
    return "auth-" + hashlib.sha256(state.encode("ascii")).hexdigest()


def prepare_private_directory(value):
    try:
        os.makedirs(value, mode=0o700, exist_ok=True)
        fd = os.open(value, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        try:
            info = os.fstat(fd)
            if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid():
                raise ValueError("Authorization profile directory is not privately owned.")
            os.fchmod(fd, 0o700)
        finally:
            os.close(fd)
    except OSError:
        raise ValueError("Cannot secure authorization profile directory.") from None


def main(argv):
    if len(argv) == 2 and argv[0] == "--private-directory":
        prepare_private_directory(argv[1])
        return
    if len(argv) == 4 and argv[0] == "--prune-profiles":
        print(prune_stale_profiles(argv[1], argv[2], argv[3]))
        return
    if len(argv) not in (2, 3) or argv[0] not in (
        "--proxy",
        "--exit-ip",
        "--auth-url",
        "--auth-profile",
    ):
        raise ValueError("授权浏览器校验参数不正确。")
    if argv[0] != "--exit-ip" and len(argv) != 2:
        raise ValueError("授权浏览器校验参数不正确。")
    if argv[0] == "--proxy":
        print(normalize_proxy(argv[1]))
    elif argv[0] == "--auth-url":
        print(normalize_auth_url(argv[1]))
    elif argv[0] == "--auth-profile":
        print(auth_profile_tag(argv[1]))
    else:
        print(validate_exit_ip(argv[1], argv[2] if len(argv) == 3 else None))


if __name__ == "__main__":
    try:
        main(sys.argv[1:])
    except ValueError as error:
        print(str(error), file=sys.stderr)
        sys.exit(1)

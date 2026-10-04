#!/bin/bash

# macOS OpenAI authorization browser launcher for Sub2API.
# The service must pass a no-auth proxy ingress as argv[3]. There is no direct
# or default-route fallback: an unavailable or malformed proxy aborts launch.

set -eu
umask 077

BASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
PYTHON="${SUB2API_AUTH_BROWSER_PYTHON:-/usr/bin/python3}"
CURL="${SUB2API_AUTH_BROWSER_CURL:-/usr/bin/curl}"
CHROME="${SUB2API_AUTH_BROWSER_CHROME:-/Applications/Google Chrome.app/Contents/MacOS/Google Chrome}"
PROFILE_ROOT="${SUB2API_AUTH_BROWSER_PROFILE_ROOT:-$BASE_DIR/profiles}"
LOG_FILE="${SUB2API_AUTH_BROWSER_LOG_FILE:-$BASE_DIR/launcher.log}"
PROFILE_MAX_AGE_HOURS="${SUB2API_AUTH_BROWSER_PROFILE_MAX_AGE_HOURS:-72}"

if [ "${1:-}" = "--validate-proxy" ]; then
  exec "$PYTHON" "$BASE_DIR/proxy_address.py" --proxy "${2:-}"
fi
if [ "${1:-}" = "--validate-auth-url" ]; then
  exec "$PYTHON" "$BASE_DIR/proxy_address.py" --auth-url "${2:-}"
fi

NAME="${1:-}"
AUTH_URL="${2:-}"
RAW_PROXY="${3:-}"
PINNED_EXIT_IP="${4:-}"
if [ "$#" -gt 4 ] || { [ "$#" -eq 4 ] && [ -z "$PINNED_EXIT_IP" ]; }; then
  echo "重新授权必须提供该账号原登录 IP，不能使用空绑定。" >&2
  exit 1
fi

if [ -z "$NAME" ] || [ -z "$AUTH_URL" ] || [ -z "$RAW_PROXY" ]; then
  echo "用法: $0 <授权环境标识> <OpenAI授权链接> <免认证代理地址:端口>" >&2
  exit 1
fi
case "$NAME" in
*[!A-Za-z0-9._-]* | . | ..)
  echo "授权环境标识只能包含英文字母、数字、点、下划线和短横线，且不能是 . 或 ..。" >&2
  exit 1
  ;;
esac
if [ "${#NAME}" -gt 100 ]; then
  echo "授权环境标识不能超过 100 个字符。" >&2
  exit 1
fi

AUTH_URL="$("$PYTHON" "$BASE_DIR/proxy_address.py" --auth-url "$AUTH_URL")" || exit 1
EXPECTED_NAME="$("$PYTHON" "$BASE_DIR/proxy_address.py" --auth-profile "$AUTH_URL")" || exit 1
if [ "$NAME" != "$EXPECTED_NAME" ]; then
  echo "授权环境标识与 OpenAI state 指纹不匹配，拒绝复用错误的浏览器配置。" >&2
  exit 1
fi
PROXY="$("$PYTHON" "$BASE_DIR/proxy_address.py" --proxy "$RAW_PROXY")" || exit 1

if [ ! -x "$CHROME" ]; then
  echo "未找到可执行的 Google Chrome。" >&2
  exit 1
fi
if [ ! -x "$CURL" ]; then
  echo "未找到可执行的 curl。" >&2
  exit 1
fi

PROFILE_DIR="$PROFILE_ROOT/$NAME"
"$PYTHON" "$BASE_DIR/proxy_address.py" --prune-profiles \
  "$PROFILE_ROOT" "$NAME" "$PROFILE_MAX_AGE_HOURS" >/dev/null
if [ -L "$PROFILE_DIR" ]; then
  echo "授权浏览器配置目录不能是符号链接。" >&2
  exit 3
fi
if [ -e "$PROFILE_DIR/SingletonLock" ] || [ -L "$PROFILE_DIR/SingletonLock" ]; then
  if [ ! -L "$PROFILE_DIR/SingletonLock" ]; then
    echo "该授权浏览器锁状态不明确，请关闭对应窗口后重试。" >&2
    exit 3
  fi
  LOCK_TARGET="$(readlink "$PROFILE_DIR/SingletonLock")"
  LOCK_PID="${LOCK_TARGET##*-}"
  case "$LOCK_PID" in
  '' | *[!0-9]*)
    echo "该授权浏览器锁状态不明确，请关闭对应窗口后重试。" >&2
    exit 3
    ;;
  esac
  if kill -0 "$LOCK_PID" 2>/dev/null; then
    echo "该授权浏览器仍在运行，请先关闭对应窗口后重试。" >&2
    exit 3
  fi
fi

CURL_PROXY="$PROXY"
case "$PROXY" in
socks5://*) CURL_PROXY="socks5h://${PROXY#socks5://}" ;;
esac
if ! EXIT_IP="$("$CURL" -q --fail --silent --show-error --max-time 12 --connect-timeout 6 \
  --noproxy "" --proxy "$CURL_PROXY" https://api.ipify.org 2>/dev/null)"; then
  echo "【中止】授权代理无法连通，未切换到直连。" >&2
  exit 2
fi
EXIT_CHECK_ARGS=(--exit-ip "$EXIT_IP" "$PROXY")
if [ "$#" -eq 4 ]; then
  EXIT_CHECK_ARGS+=("$PINNED_EXIT_IP")
fi
if ! EXIT_IP="$("$PYTHON" "$BASE_DIR/proxy_address.py" "${EXIT_CHECK_ARGS[@]}" 2>/dev/null)"; then
  echo "【中止】授权代理出口身份校验失败，未启动浏览器。" >&2
  exit 2
fi

mkdir -p "$PROFILE_DIR/Default" "$(dirname "$LOG_FILE")"
if [ ! -f "$PROFILE_DIR/Default/Preferences" ]; then
  cat >"$PROFILE_DIR/Default/Preferences" <<'EOF'
{"intl":{"accept_languages":"en-US,en"},"dns_over_https":{"mode":"secure","templates":"https://cloudflare-dns.com/dns-query"}}
EOF
fi

printf '%s LAUNCH name=%s exit=%s\n' "$(date '+%F %T')" "$NAME" "$EXIT_IP" >>"$LOG_FILE"
TZ="America/New_York" nohup "$CHROME" \
  --user-data-dir="$PROFILE_DIR" \
  --proxy-server="$PROXY" \
  --lang=en-US \
  --accept-lang=en-US \
  --force-webrtc-ip-handling-policy=disable_non_proxied_udp \
  --disable-quic \
  --no-first-run \
  --no-default-browser-check \
  "$AUTH_URL" >/dev/null 2>&1 &
CHROME_PID=$!
sleep 1
CHROME_STATE="$(/bin/ps -p "$CHROME_PID" -o stat= 2>/dev/null || true)"
case "$CHROME_STATE" in
'' | Z*)
  echo "授权浏览器启动进程已退出，请关闭已有窗口后重试。" >&2
  exit 3
  ;;
esac

echo "已启动 OpenAI 授权浏览器：环境=$NAME 代理=$PROXY 出口=$EXIT_IP"

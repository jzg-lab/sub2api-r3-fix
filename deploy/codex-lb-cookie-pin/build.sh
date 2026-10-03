#!/bin/sh
# build.sh — 多平台构建 + 打包 .s2plugin
#
# 用法：
#   S2PLUGIN_KEY=/existing/publisher.key ./build.sh --release
#   ./build.sh --allow-unsigned   # 仅限隔离的本地开发
#
# 平台矩阵：darwin-arm64、darwin-amd64、linux-amd64、linux-arm64、windows-amd64。
# 依赖拉取走 goproxy.cn（本机直连 proxy.golang.org 被墙）。
set -eu
cd "$(dirname "$0")"

export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"
export CGO_ENABLED=0

# Reject an incomplete release before tests, cross-compilation or replacement.
mode="${1:---release}"
if [ "$#" -gt 1 ]; then
  echo "usage: build.sh [--release|--allow-unsigned]" >&2
  exit 2
fi
case "$mode" in
  --release)
    if [ -z "${S2PLUGIN_KEY:-}" ] || [ ! -f "$S2PLUGIN_KEY" ] || [ ! -r "$S2PLUGIN_KEY" ]; then
      echo "release requires a readable existing S2PLUGIN_KEY; unsigned output is development-only" >&2
      exit 2
    fi
    go run ./tools/packager -check-key -key "$S2PLUGIN_KEY"
    ;;
  --allow-unsigned)
    if [ -n "${S2PLUGIN_KEY:-}" ]; then
      echo "--allow-unsigned cannot be combined with S2PLUGIN_KEY" >&2
      exit 2
    fi
    ;;
  *)
    echo "usage: build.sh [--release|--allow-unsigned]" >&2
    exit 2
    ;;
esac

echo "== 单测 =="
go test ./...

echo "== 构建 =="
for target in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64; do
  goos="${target%/*}"
  goarch="${target#*/}"
  out="dist/runtimes/${goos}-${goarch}/cookiepin"
  if [ "$goos" = windows ]; then
    out="${out}.exe"
  fi
  mkdir -p "$(dirname "$out")"
  GOOS="$goos" GOARCH="$goarch" go build -trimpath -ldflags "-s -w" -o "$out" ./cmd/cookiepin
  echo "  ${goos}-${goarch}: $(wc -c < "$out") bytes"
done

echo "== 打包 =="
if [ "$mode" = --release ]; then
  go run ./tools/packager -runtimes dist/runtimes -ui ui -key "$S2PLUGIN_KEY" -key-id "${S2PLUGIN_KEY_ID:-}"
else
  go run ./tools/packager -runtimes dist/runtimes -ui ui -allow-unsigned
fi

echo "== 冒烟（本机模拟宿主全链路）==="
native_os="$(go env GOOS)"
native_binary="dist/runtimes/${native_os}-$(go env GOARCH)/cookiepin"
if [ "$native_os" = windows ]; then
  native_binary="${native_binary}.exe"
fi
go run ./tools/testhost -plugin "$native_binary" -mock

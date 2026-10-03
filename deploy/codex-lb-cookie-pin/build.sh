#!/bin/sh
# build.sh — 多平台构建 + 打包 .s2plugin
#
# 用法：
#   ./build.sh                    # 构建全部平台 + 打未签名包
#   S2PLUGIN_KEY=~/.s2plugin-keys/cookiepin.ed25519 ./build.sh   # 带签名打包
#
# 平台矩阵：darwin-arm64、darwin-amd64、linux-amd64、linux-arm64、windows-amd64。
# 依赖拉取走 goproxy.cn（本机直连 proxy.golang.org 被墙）。
set -eu
cd "$(dirname "$0")"

export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"
export CGO_ENABLED=0

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
if [ -n "${S2PLUGIN_KEY:-}" ]; then
  go run ./tools/packager -runtimes dist/runtimes -ui ui -key "$S2PLUGIN_KEY"
else
  go run ./tools/packager -runtimes dist/runtimes -ui ui
fi

echo "== 冒烟（本机模拟宿主全链路）==="
native_os="$(go env GOOS)"
native_binary="dist/runtimes/${native_os}-$(go env GOARCH)/cookiepin"
if [ "$native_os" = windows ]; then
  native_binary="${native_binary}.exe"
fi
go run ./tools/testhost -plugin "$native_binary" -mock

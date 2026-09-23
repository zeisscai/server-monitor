#!/usr/bin/env bash
#
# 交叉编译脚本：生成无 CGO 依赖的单二进制（Linux amd64 / arm64）。
#
# 用法：
#   ./build.sh            # 构建发布产物到 dist/
#   VERSION=1.2.0 ./build.sh

set -euo pipefail

cd "$(dirname "$0")"

VERSION="${VERSION:-1.0.0}"
OUT_DIR="dist"

if ! command -v go >/dev/null 2>&1; then
  if [ -x /usr/local/go/bin/go ]; then
    PATH="/usr/local/go/bin:$PATH"
  else
    echo "未找到 go 命令，请先安装 Go 1.22 或更高版本" >&2
    exit 1
  fi
fi

mkdir -p "$OUT_DIR"

# 纯 Go 实现（modernc.org/sqlite），关闭 CGO 即可交叉编译为静态单二进制。
export CGO_ENABLED=0
export GOFLAGS="-trimpath"
LDFLAGS="-s -w"

targets=(
  "linux/amd64"
  "linux/arm64"
  "linux/arm"
  "darwin/arm64"
  "darwin/amd64"
)

for t in "${targets[@]}"; do
  GOOS="${t%%/*}"
  GOARCH="${t##*/}"
  name="server-monitor-${GOOS}-${GOARCH}"
  echo "==> 构建 ${name}"
  GOOS="$GOOS" GOARCH="$GOARCH" go build -ldflags "$LDFLAGS" -o "${OUT_DIR}/${name}" .
done

cp -f deploy/install.sh  "${OUT_DIR}/install.sh"
cp -f deploy/diagnose.sh "${OUT_DIR}/diagnose.sh"
chmod +x "${OUT_DIR}/install.sh" "${OUT_DIR}/diagnose.sh"

echo
echo "构建完成（版本 ${VERSION}），产物位于 ${OUT_DIR}/："
ls -lh "${OUT_DIR}"

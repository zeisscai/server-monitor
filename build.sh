#!/usr/bin/env bash
#
# 交叉编译脚本：生成无 CGO 依赖的单二进制（Linux amd64 / arm64 / arm），
# 并写出 SHA256SUMS.txt 供发布与安装脚本校验。
#
# 用法：
#   ./build.sh                 # 版本号取自 git tag（v1.1.0 -> 1.1.0），无 tag 时用 main.go 里的默认值
#   VERSION=1.2.0 ./build.sh   # 显式指定版本
#
# 发布流程见 README 第 13 节：build -> git tag -> 上传 dist/ 下的产物到 GitHub Release。

set -euo pipefail

cd "$(dirname "$0")"

if ! command -v go >/dev/null 2>&1; then
  if [ -x /usr/local/go/bin/go ]; then
    PATH="/usr/local/go/bin:$PATH"
  else
    echo "未找到 go 命令，请先安装 Go 1.22 或更高版本" >&2
    exit 1
  fi
fi

# 版本号优先级：环境变量 > git tag > 源码默认值
if [ -z "${VERSION:-}" ]; then
  VERSION="$(git describe --tags --abbrev=0 2>/dev/null | sed 's/^v//' || true)"
fi
if [ -z "${VERSION:-}" ]; then
  VERSION="$(sed -n 's/^var version = "\(.*\)"$/\1/p' main.go | head -n1)"
fi
[ -n "${VERSION:-}" ] || { echo "无法确定版本号，请用 VERSION=x.y.z ./build.sh 指定" >&2; exit 1; }

OUT_DIR="dist"
mkdir -p "$OUT_DIR"

# 纯 Go 实现（modernc.org/sqlite），关闭 CGO 即可交叉编译为静态单二进制。
export CGO_ENABLED=0
export GOFLAGS="-trimpath"
LDFLAGS="-s -w -X main.version=${VERSION}"

targets=(
  "linux/amd64"
  "linux/arm64"
  "linux/arm"
  "darwin/arm64"
  "darwin/amd64"
)

names=()
for t in "${targets[@]}"; do
  GOOS="${t%%/*}"
  GOARCH="${t##*/}"
  name="server-monitor-${GOOS}-${GOARCH}"
  echo "==> 构建 ${name}"
  GOOS="$GOOS" GOARCH="$GOARCH" go build -ldflags "$LDFLAGS" -o "${OUT_DIR}/${name}" .
  names+=("${name}")
done

# 用「与构建机同平台」的产物跑一次 version，确认 ldflags 注入生效。
# 不能直接跑 linux 产物：在 macOS 上构建时那些文件无法执行。
host_os="$(uname -s | tr 'A-Z' 'a-z')"
host_arch="$(uname -m)"
case "$host_arch" in
  x86_64|amd64)  host_arch="amd64" ;;
  aarch64|arm64) host_arch="arm64" ;;
  armv7l|armv7)  host_arch="arm" ;;
esac
probe="${OUT_DIR}/server-monitor-${host_os}-${host_arch}"
if [ -x "$probe" ]; then
  built="$("$probe" version 2>/dev/null || true)"
  case "$built" in
    *"v${VERSION}") echo "==> 版本校验通过：${built}" ;;
    *) echo "警告：产物版本号与预期不符（期望 v${VERSION}，实际 ${built:-读取失败}）" >&2 ;;
  esac
else
  echo "==> 跳过版本校验（构建机上没有 ${host_os}/${host_arch} 产物）"
fi

# 校验和：Linux 用 sha256sum，macOS 只有 shasum。
(
  cd "$OUT_DIR"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "${names[@]}" > SHA256SUMS.txt
  else
    shasum -a 256 "${names[@]}" > SHA256SUMS.txt
  fi
)

cp -f install.sh         "${OUT_DIR}/install.sh"
cp -f deploy/diagnose.sh "${OUT_DIR}/diagnose.sh"
chmod +x "${OUT_DIR}/install.sh" "${OUT_DIR}/diagnose.sh"

echo
echo "构建完成（版本 ${VERSION}），产物位于 ${OUT_DIR}/："
ls -lh "${OUT_DIR}"
echo
echo "发布："
echo "  git tag -a v${VERSION} -m 'v${VERSION}' && git push origin v${VERSION}"
echo "  然后把 ${OUT_DIR}/ 下的二进制、SHA256SUMS.txt、install.sh 上传到该 Release"

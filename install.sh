#!/usr/bin/env bash
#
# 服务器监控 — 一键安装 / 升级 / 管理脚本
#
# 不需要先下载二进制：脚本会按 CPU 架构从 GitHub Release 自动取对应版本并校验 SHA-256。
#
# 用法（在服务器终端直接执行，无需先保存文件）：
#   bash <(curl -fsSL https://raw.githubusercontent.com/zeisscai/server-monitor/main/install.sh)
#   国内网络可加镜像前缀：
#   SMD_MIRROR=https://ghfast.top/ bash <(curl -fsSL ...)
#
# 也可以带子命令直接执行（便于批量部署 / 自动化）：
#   install.sh               进入交互菜单
#   install.sh install       全新安装（缺省项走默认值）
#   install.sh upgrade       升级到最新版本，保留配置与历史数据
#   install.sh manage        管理菜单（启停 / 状态 / 日志 / 改端口 / 改密码 / 卸载）
#   install.sh status        查看运行状态
#   install.sh uninstall     卸载
#
# install / upgrade 支持的参数：
#   --port N      Web 端口（默认沿用已有配置，首次为 8080）
#   --user U      登录账号（默认 smdadmin）
#   --pass P      登录密码（默认随机生成）
#   --bin PATH    使用本地二进制文件，不联网下载（离线部署）
#   --version V   指定版本，如 1.1.0 或 v1.1.0（默认最新）
#   --yes         全部使用默认值 / 不再逐项确认
#
# 环境变量：
#   SMD_MIRROR   下载镜像前缀，如 https://ghfast.top/（仅影响下载地址）
#   SMD_APP_DIR  自定义安装目录，默认 /opt/server-monitor
#
# 路径约定：程序二进制、配置文件、数据目录统一位于安装目录下；
# 只有 systemd 单元固定放在 /etc/systemd/system/（systemd 规定）。

set -uo pipefail

# ---------------- 固定约定 ----------------
APP_NAME="server-monitor"
APP_DIR="${SMD_APP_DIR:-/opt/server-monitor}"
BIN="${APP_DIR}/${APP_NAME}"
CONF="${APP_DIR}/config.json"
DATA="${APP_DIR}/data"
UNIT="/etc/systemd/system/${APP_NAME}.service"

# 早期版本的分散目录，仅用于升级时继承已有配置与历史数据，不要删除。
LEGACY_CONF="/etc/server-monitor/config.json"
LEGACY_DATA="/var/lib/server-monitor"

REPO="${SMD_REPO:-zeisscai/server-monitor}"
RAW_URL="https://raw.githubusercontent.com/${REPO}/main/install.sh"

DEFAULT_USER="smdadmin"
DEFAULT_PORT="8080"

# 运行期开关（--yes 由参数解析置位）
NONSTOP=0
CMD=""
OPT_PORT=""; OPT_USER=""; OPT_PASS=""; OPT_BIN=""; OPT_VERSION=""

# 颜色（非终端时自动关闭，避免把转义序列写进日志）
if [ -t 1 ]; then
  C_R=$'\033[31m'; C_G=$'\033[32m'; C_Y=$'\033[33m'; C_B=$'\033[36m'; C_D=$'\033[2m'; C_0=$'\033[0m'
else
  C_R=""; C_G=""; C_Y=""; C_B=""; C_D=""; C_0=""
fi

info() { printf '%s[信息]%s %s\n' "$C_G" "$C_0" "$*"; }
warn() { printf '%s[警告]%s %s\n' "$C_Y" "$C_0" "$*"; }
step() { printf '\n%s==> %s%s\n' "$C_B" "$*" "$C_0"; }
die()  { printf '%s[错误]%s %s\n' "$C_R" "$C_0" "$*" >&2; exit 1; }

# ---------------- 交互读取 ----------------
# 通过 `bash <(curl ...)` 或 `curl | bash` 运行时，stdin 不是终端（后者甚至是脚本文本），
# 因此所有输入都从 /dev/tty 读，保证两种启动方式都能正常交互。
if [ -r /dev/tty ] && { : </dev/tty; } 2>/dev/null; then
  TTY=/dev/tty
else
  TTY=""
fi
INTERACTIVE=0
[ -n "$TTY" ] && INTERACTIVE=1

_readline() { # 读一行到 stdout
  local v=""
  if [ -n "$TTY" ]; then
    IFS= read -r v <"$TTY" || v=""
  else
    IFS= read -r v || v=""
  fi
  printf '%s' "$v"
}

ask() { # ask <提示> [默认值]
  local prompt="$1" def="${2:-}" ans=""
  printf '%s' "$prompt" >&2
  ans="$(_readline)"
  [ -n "$ans" ] || ans="$def"
  printf '%s' "$ans"
}

ask_secret() { # ask_secret <提示>：不回声，仅在真实终端生效
  local prompt="$1" ans=""
  printf '%s' "$prompt" >&2
  if [ -n "$TTY" ] && command -v stty >/dev/null 2>&1; then
    stty -echo <"$TTY" 2>/dev/null || true
    ans="$(_readline)"
    stty echo <"$TTY" 2>/dev/null || true
    printf '\n' >&2
  else
    ans="$(_readline)"
  fi
  printf '%s' "$ans"
}

confirm() { # confirm <提示> [默认 y/N]
  local prompt="$1" def="${2:-N}" ans=""
  [ "$NONSTOP" = 1 ] && { [ "$def" = "Y" ]; return $?; }
  ans="$(ask "$prompt " "$def")"
  case "$ans" in
    [yY]|[yY][eE][sS]) return 0 ;;
    *) return 1 ;;
  esac
}

pause() {
  [ "$INTERACTIVE" = 1 ] || return 0
  [ "$NONSTOP" = 1 ] && return 0
  printf '\n%s按回车返回…%s' "$C_D" "$C_0" >&2
  _readline >/dev/null
}

# ---------------- 提权 ----------------
SELF="${BASH_SOURCE[0]:-$0}"
ARGS=("$@")
SELF_TMP=""

reexec_root() {
  [ "$(id -u)" -eq 0 ] && return 0
  if [ "${SMD_TEST_MODE:-0}" = "1" ]; then
    warn "测试模式：跳过提权检查"
    return 0
  fi
  command -v sudo >/dev/null 2>&1 || die "需要 root 权限，且系统没有 sudo。请用 root 账户执行。"

  # `bash <(curl ...)` 时脚本路径是 /dev/fd/NN，sudo 起的子进程读不到这个 fd，
  # 先落一份临时副本再提权。
  local self="$SELF"
  if [ ! -f "$self" ] || [ ! -r "$self" ]; then
    SELF_TMP="$(mktemp "${TMPDIR:-/tmp}/smd-install.XXXXXX")" || die "无法创建临时文件"
    if ! cat "$SELF" >"$SELF_TMP" 2>/dev/null; then
      dl_to "$RAW_URL" "$SELF_TMP" || die "无法准备提权所需的脚本副本，请手动下载后运行"
    fi
    self="$SELF_TMP"
    trap 'rm -f "$SELF_TMP"' EXIT
  fi
  printf '%s需要管理员权限，正在通过 sudo 重新执行…%s\n' "$C_D" "$C_0"
  # 不用 sudo -E：多数发行版默认禁止保留环境变量。需要传递的配置逐项显式带入。
  exec sudo env \
    SMD_MIRROR="${SMD_MIRROR:-}" \
    SMD_APP_DIR="$APP_DIR" \
    SMD_REPO="$REPO" \
    bash "$self" ${ARGS[@]+"${ARGS[@]}"}
}

# ---------------- 下载 ----------------
dl_to() { # dl_to <url> <目标文件>
  if command -v curl >/dev/null 2>&1; then
    curl -fL --connect-timeout 15 --max-time 600 --retry 2 -o "$2" "$1"
  elif command -v wget >/dev/null 2>&1; then
    wget -q --timeout=30 --tries=3 -O "$2" "$1"
  else
    return 127
  fi
}

fetch_text() { # fetch_text <url> -> stdout
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL --connect-timeout 15 --max-time 30 "$1"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO- --timeout=30 "$1"
  else
    return 127
  fi
}

url_effective() { # 跟随重定向拿到最终地址（用于解析最新版本号，不消耗 API 配额）
  if command -v curl >/dev/null 2>&1; then
    curl -sIL -o /dev/null -w '%{url_effective}' --connect-timeout 15 --max-time 30 "$1"
  else
    fetch_text "$1" >/dev/null 2>&1 || true
    printf ''
  fi
}

sha256_of() { # sha256_of <文件> -> hex
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

# ---------------- 环境与版本 ----------------
detect_arch() {
  local m
  m="$(uname -m)"
  case "$m" in
    x86_64|amd64)   echo "amd64" ;;
    aarch64|arm64)  echo "arm64" ;;
    armv7l|armv7|armv6l|arm) echo "arm" ;;
    *) die "不支持的 CPU 架构：${m}（支持 amd64 / arm64 / arm）" ;;
  esac
}

preflight() {
  step "环境检查"
  if [ "${SMD_TEST_MODE:-0}" != "1" ]; then
    [ "$(uname -s)" = "Linux" ] || die "本程序运行在 Linux 上，当前系统是 $(uname -s)。"
    command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ] \
      || die "未检测到 systemd（systemctl 或 /run/systemd/system 缺失），无法注册服务。"
    command -v curl >/dev/null 2>&1 || command -v wget >/dev/null 2>&1 \
      || die "系统缺少 curl 或 wget，无法下载安装包。请先安装其中之一。"
    info "systemd 可用"
    if [ "$INTERACTIVE" = 0 ]; then
      warn "当前没有可用的终端（/dev/tty），交互提示将使用默认值。"
    fi
  else
    warn "测试模式：跳过 Linux / systemd / 网络检查"
  fi
  ARCH="$(detect_arch)"
  info "系统：$(uname -s) $(uname -r)  架构：${ARCH}"
}

installed() { [ -x "$BIN" ]; }

installed_version() {
  installed || return 1
  "$BIN" version 2>/dev/null | sed -n 's/.*v\([0-9][^ ]*\).*/\1/p' | head -n1
}

normalize_ver() { printf '%s' "${1#v}"; }

resolve_latest() {
  local want="${1:-}" tag="" loc
  if [ -n "$want" ]; then
    normalize_ver "$want"
    return 0
  fi
  loc="$(url_effective "https://github.com/${REPO}/releases/latest")"
  tag="$(printf '%s' "$loc" | sed -n 's#.*/releases/tag/##p')"
  if [ -z "$tag" ]; then
    # 回退到 API（未认证每小时 60 次，仅在上面的方式失败时用）
    tag="$(fetch_text "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null \
           | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1)"
  fi
  [ -n "$tag" ] || die "无法获取最新版本号：请检查网络，或用 --version 指定版本、--bin 指定本地文件。"
  normalize_ver "$tag"
}

ver_gt() { # ver_gt A B -> A 是否比 B 新（按 x.y.z 数值比较，缺位补 0，兼容 BSD/macOS 的 sort）
  local i a b
  local -a av bv
  IFS='.' read -r -a av <<<"$1"
  IFS='.' read -r -a bv <<<"$2"
  for i in 0 1 2; do
    a="${av[$i]:-0}"; b="${bv[$i]:-0}"
    a="${a//[!0-9]/}"; b="${b//[!0-9]/}"
    a="${a:-0}"; b="${b:-0}"
    if [ "$((10#$a))" -gt "$((10#$b))" ]; then return 0; fi
    if [ "$((10#$a))" -lt "$((10#$b))" ]; then return 1; fi
  done
  return 1
}

# ---------------- 配置读取 ----------------
json_str() {
  [ -f "$CONF" ] || return 0
  grep -m1 "\"$1\"" "$CONF" 2>/dev/null | sed -e 's/^[^:]*:[[:space:]]*"//' -e 's/",\{0,1\}[[:space:]]*$//'
}

json_num() {
  [ -f "$CONF" ] || return 0
  grep -m1 "\"$1\"" "$CONF" 2>/dev/null | sed -e 's/^[^:]*:[[:space:]]*//' -e 's/,[[:space:]]*$//' | tr -d ' '
}

json_bool() {
  [ -f "$CONF" ] || return 0
  grep -m1 "\"$1\"" "$CONF" 2>/dev/null | sed -e 's/^[^:]*:[[:space:]]*//' -e 's/,[[:space:]]*$//' | tr -d ' '
}

# ---------------- 端口与密码 ----------------
port_in_use() {
  local port="$1"
  if command -v ss >/dev/null 2>&1; then
    ss -lnt 2>/dev/null | awk -v p=":${port}" 'NR>1 && index($4, p) { found=1 } END { exit(found ? 0 : 1) }'
  elif command -v lsof >/dev/null 2>&1; then
    lsof -nP -iTCP:"${port}" -sTCP:LISTEN >/dev/null 2>&1
  else
    return 1
  fi
}

port_owner_is_self() {
  local port="$1"
  command -v ss >/dev/null 2>&1 || return 1
  ss -lntp 2>/dev/null | awk -v p=":${port}" -v n="${APP_NAME}" \
    'NR>1 && index($4, p) && index($0, n) { found=1 } END { exit(found ? 0 : 1) }'
}

port_show() {
  local port="$1"
  if command -v ss >/dev/null 2>&1; then
    ss -lntp 2>/dev/null | awk -v p=":${port}" 'NR>1 && index($4, p)' | sed 's/^/    /'
  elif command -v lsof >/dev/null 2>&1; then
    lsof -nP -iTCP:"${port}" -sTCP:LISTEN 2>/dev/null | sed 's/^/    /'
  fi
  return 0
}

gen_password() {
  head -c 12 /dev/urandom | od -An -tx1 | tr -d ' \n' | cut -c1-16
}

# 校验一个候选端口能否使用；不可用时说明原因并返回 1。
port_ok() {
  local p="$1" recv
  case "$p" in ''|*[!0-9]*) warn "端口必须是数字"; return 1 ;; esac
  p="$((10#$p))"
  [ "$p" -ge 1 ] && [ "$p" -le 65535 ] || { warn "端口需在 1~65535 之间"; return 1; }
  [ "$p" -ge 1024 ] || warn "低于 1024 的端口属于特权端口，服务以普通用户运行会失败"
  recv="$(json_num receive_port)"
  if [ -n "$recv" ] && [ "$p" = "$recv" ]; then warn "与数据接收端口 ${recv} 冲突"; return 1; fi
  if port_in_use "$p" && ! port_owner_is_self "$p"; then
    warn "端口 ${p} 已被占用："; port_show "$p"
    confirm "仍要使用该端口？[y/N]" "N" || return 1
  fi
  return 0
}

# 与 Go 侧 config.ValidatePassword 保持一致：先在录入阶段拦下，避免写配置时才失败。
check_password() {
  local user="$1" pass="$2" lower first
  [ "${#pass}" -ge 8 ] || { warn "密码至少 8 个字符"; return 1; }
  [ "${#pass}" -le 128 ] || { warn "密码不能超过 128 个字符"; return 1; }
  lower="$(printf '%s' "$pass" | tr '[:upper:]' '[:lower:]')"
  case "$lower" in
    password|passw0rd|password1|12345678|123456789|1234567890|123456|qwertyui|qwerty123|iloveyou|letmein1|admin123|administrator|changeme|welcome1|smdadmin|servermonitor|monitor123|abc12345|11111111|00000000|88888888)
      warn "这是常见弱口令，请换一个"; return 1 ;;
  esac
  if [ -n "$user" ] && [ "$lower" = "$(printf '%s' "$user" | tr '[:upper:]' '[:lower:]')" ]; then
    warn "密码不能与登录账号相同"; return 1
  fi
  first="${pass:0:1}"
  [ -n "${pass//"$first"/}" ] || { warn "密码不能是同一个字符的重复"; return 1; }
  return 0
}

# ---------------- 部署动作 ----------------
prepare_dirs() {
  mkdir -p "$APP_DIR" "$DATA" || die "无法创建目录 ${APP_DIR}"
  chmod 755 "$APP_DIR" "$DATA"
  # 兼容早期分散布局：仅在新位置为空时继承，绝不覆盖现有数据。
  if [ "$LEGACY_CONF" != "$CONF" ] && [ -f "$LEGACY_CONF" ] && [ ! -f "$CONF" ]; then
    cp -p "$LEGACY_CONF" "$CONF" && info "已从 ${LEGACY_CONF} 继承原有配置"
  fi
  if [ "$LEGACY_DATA" != "$DATA" ] && [ -d "$LEGACY_DATA" ] && [ -z "$(ls -A "$DATA" 2>/dev/null)" ]; then
    cp -a "$LEGACY_DATA"/. "$DATA"/ || die "继承历史数据失败，已终止以避免数据丢失"
    info "已从 ${LEGACY_DATA} 继承历史数据库"
  fi
}

write_unit() {
  cat >"$UNIT" <<EOF || die "写入 systemd 单元失败：$UNIT"
[Unit]
Description=Server Monitor
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=${BIN} --config ${CONF} --data-dir ${DATA}
Restart=always
RestartSec=5
# 千台上报时每台保持一条长连接，默认 1024 个句柄不够用。
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
EOF
  chmod 644 "$UNIT"
  systemctl daemon-reload 2>/dev/null || true
}

# 以服务身份前台试跑几秒：配置能否读取、数据库能否写入、端口能否监听，
# 任何一步失败都会立刻打印程序自己的错误，而不是进入无声的崩溃重启循环。
smoke_test() {
  command -v timeout >/dev/null 2>&1 || { warn "未找到 timeout，跳过启动前自检"; return 0; }
  local log rc=0 out=""
  log="$(mktemp "${TMPDIR:-/tmp}/smd-smoke.XXXXXX")"
  timeout 3 "$BIN" --config "$CONF" --data-dir "$DATA" >"$log" 2>&1 || rc=$?
  out="$(cat "$log" 2>/dev/null || true)"
  rm -f "$log"
  if [ "$rc" -ne 0 ] && [ "$rc" -ne 124 ] && [ "$rc" -ne 143 ]; then
    printf '%s\n' "$out" >&2
    return 1
  fi
  printf '%s' "$out" | grep -qE '启动失败|服务异常退出' && { printf '%s\n' "$out" >&2; return 1; }
  sleep 1   # 等优雅退出真正释放端口，避免影响随后 systemd 启动
  return 0
}

svc_start() {
  systemctl enable "$APP_NAME" >/dev/null 2>&1 || true
  systemctl restart "$APP_NAME" || return 1
  local i
  for i in $(seq 1 15); do
    systemctl is-active --quiet "$APP_NAME" && return 0
    sleep 1
  done
  return 1
}

svc_active() { systemctl is-active --quiet "$APP_NAME" 2>/dev/null; }

# systemctl is-active / is-enabled 在「未激活」时也会往 stdout 打印 inactive 并非 0 退出，
# 直接写 `$(... || echo unknown)` 会输出两行，所以统一走这两个函数。
svc_state() {
  local s
  s="$(systemctl is-active "$APP_NAME" 2>/dev/null)"
  [ -n "$s" ] || s="unknown"
  printf '%s' "$s"
}

svc_enabled() {
  local s
  s="$(systemctl is-enabled "$APP_NAME" 2>/dev/null)"
  [ -n "$s" ] || s="unknown"
  printf '%s' "$s"
}

health_check() { # 服务起来后用 HTTP 探一次，确认真的在服务（而不只是进程活着）
  local port code
  port="$(json_num web_port)"
  [ -n "$port" ] || return 0
  command -v curl >/dev/null 2>&1 || return 0
  code="$(curl -s -o /dev/null -m 5 -w '%{http_code}' "http://127.0.0.1:${port}/" 2>/dev/null || true)"
  case "$code" in
    200|301|302) return 0 ;;
    *) return 1 ;;
  esac
}

service_fail_hint() {
  printf '\n%s最近日志：%s\n' "$C_D" "$C_0" >&2
  journalctl -u "$APP_NAME" -n 30 --no-pager >&2 2>/dev/null || true
  printf '\n排查：%s\n' "install.sh manage → 系统诊断" >&2
}

# ---------------- 安装 ----------------
read_common_inputs() { # 结果写入 WEB_PORT / WEB_USER / WEB_PASS / BIN_SRC
  local def_port def_user ans
  def_port="$(json_num web_port)"
  [ -n "$def_port" ] || def_port="$DEFAULT_PORT"
  def_user="$(json_str web_user)"
  [ -n "$def_user" ] || def_user="$DEFAULT_USER"

  if [ -n "${OPT_PORT:-}" ]; then
    port_ok "$OPT_PORT" || die "端口 ${OPT_PORT} 不可用（见上方原因）"
    OPT_PORT="$((10#$OPT_PORT))"
  elif [ "$NONSTOP" = 1 ]; then
    OPT_PORT="$def_port"
  else
    while :; do
      OPT_PORT="$(ask "Web 监听端口 [${def_port}]: " "$def_port")"
      port_ok "$OPT_PORT" || continue
      OPT_PORT="$((10#$OPT_PORT))"
      break
    done
  fi

  if [ -z "${OPT_USER:-}" ]; then
    if [ "$NONSTOP" = 1 ]; then OPT_USER="$def_user"; else OPT_USER="$(ask "登录账号 [${def_user}]: " "$def_user")"; fi
  fi
  [ -n "$OPT_USER" ] || die "登录账号不能为空"

  if [ -z "${OPT_PASS:-}" ]; then
    # 已有配置时默认沿用原密码：重装/覆盖安装不应悄悄改掉管理员正在用的口令。
    local existing fallback note
    existing="$(json_str web_pass)"
    if [ -n "$existing" ]; then
      fallback="$existing"
      note="直接回车沿用现有密码"
    else
      fallback="$(gen_password)"
      note="直接回车随机生成"
    fi
    if [ "$NONSTOP" = 1 ]; then
      OPT_PASS="$fallback"
      if [ -n "$existing" ]; then
        info "登录密码：沿用现有配置"
      else
        info "已随机生成登录密码"
      fi
    else
      while :; do
        OPT_PASS="$(ask_secret "登录密码（至少 8 位，${note}）: ")"
        if [ -z "$OPT_PASS" ]; then OPT_PASS="$fallback"; break; fi
        check_password "$OPT_USER" "$OPT_PASS" && break
        warn "请重新输入，或直接回车使用默认值"
      done
    fi
  fi

  WEB_PORT="$OPT_PORT"; WEB_USER="$OPT_USER"; WEB_PASS="$OPT_PASS"
}

acquire_binary() { # 产出 BIN_SRC，必要时下载并校验
  local dest_dir="$1" arch tag url want sums_url sums_file expect actual self_ver
  if [ -n "${OPT_BIN:-}" ]; then
    [ -f "$OPT_BIN" ] || die "指定的二进制文件不存在：${OPT_BIN}"
    BIN_SRC="$OPT_BIN"
    info "使用本地二进制：${BIN_SRC}"
    return 0
  fi

  tag="$(resolve_latest "${OPT_VERSION:-}")"
  want="$tag"
  arch="${ARCH:-$(detect_arch)}"
  url="${SMD_MIRROR:-}https://github.com/${REPO}/releases/download/v${tag}/${APP_NAME}-linux-${arch}"
  BIN_SRC="${dest_dir}/${APP_NAME}-linux-${arch}"

  step "下载 ${APP_NAME} v${tag}（linux/${arch}）"
  info "来源：${url}"
  if ! dl_to "$url" "$BIN_SRC"; then
    rm -f "$BIN_SRC"
    die "下载失败。可尝试：1) 设置镜像 SMD_MIRROR=https://ghfast.top/ 2) 用 --bin 指定本地文件"
  fi
  chmod +x "$BIN_SRC"
  [ -s "$BIN_SRC" ] || die "下载到的文件为空：${url}"

  # 校验和：能取到就强制比对，取不到时只提示（支持自建镜像 / 离线分发）
  sums_url="${SMD_MIRROR:-}https://github.com/${REPO}/releases/download/v${tag}/SHA256SUMS.txt"
  sums_file="${dest_dir}/SHA256SUMS.txt"
  if dl_to "$sums_url" "$sums_file" 2>/dev/null; then
    expect="$(awk -v f="${APP_NAME}-linux-${arch}" '$2 == f || $2 == "*"f {print $1}' "$sums_file" | head -n1)"
    if [ -n "$expect" ]; then
      actual="$(sha256_of "$BIN_SRC")"
      [ "$expect" = "$actual" ] || die "SHA-256 校验失败（期望 ${expect}，实际 ${actual}），已停止安装"
      info "SHA-256 校验通过"
    else
      warn "校验文件中没有该架构的条目，跳过校验"
    fi
  else
    warn "未取到 SHA256SUMS.txt，跳过校验"
  fi

  # 确认下载到的确实是本程序且版本匹配
  self_ver="$("$BIN_SRC" version 2>/dev/null | sed -n 's/.*v\([0-9][^ ]*\).*/\1/p')"
  [ -n "$self_ver" ] || die "下载的文件无法执行（架构不匹配或不是本程序）：${BIN_SRC}"
  [ "$self_ver" = "$want" ] || warn "产物自报版本 ${self_ver} 与期望 ${want} 不一致"
}

cmd_install() {
  printf '%s服务器监控 · 安装%s\n' "$C_B" "$C_0"
  reexec_root
  preflight
  if installed; then
    warn "检测到已安装（版本 $(installed_version 2>/dev/null || echo 未知)）：${BIN}"
    info "只想更新程序请用：install.sh upgrade（保留配置与数据，失败自动回滚）"
    if [ "$NONSTOP" != 1 ]; then
      confirm "仍要重新执行安装（覆盖程序，保留配置与数据）？[y/N]" "N" || die "已取消"
    fi
  fi

  read_common_inputs

  local tmpdir
  tmpdir="$(mktemp -d "${TMPDIR:-/tmp}/smd-dl.XXXXXX")"
  # shellcheck disable=SC2064
  trap "rm -rf '$tmpdir'" EXIT
  acquire_binary "$tmpdir"

  printf '\n请确认安装信息：\n'
  printf '  安装目录 : %s\n' "$APP_DIR"
  printf '  配置文件 : %s\n' "$CONF"
  printf '  数据目录 : %s\n' "$DATA"
  printf '  Web 端口 : %s\n' "$WEB_PORT"
  printf '  登录账号 : %s\n' "$WEB_USER"
  confirm "确认开始安装？[Y/n]" "Y" || die "已取消"

  step "创建目录"
  prepare_dirs
  info "安装目录：${APP_DIR}"

  step "停止旧服务"
  systemctl stop "$APP_NAME" 2>/dev/null || true

  step "安装程序"
  install -m 0755 "$BIN_SRC" "$BIN" || die "安装二进制失败"
  info "已安装到 ${BIN}"

  step "写入配置"
  "$BIN" init --config "$CONF" --data-dir "$DATA" \
      --web-user "$WEB_USER" --web-pass "$WEB_PASS" --web-port "$WEB_PORT" \
      || die "初始化配置失败"
  chmod 600 "$CONF"
  info "配置已写入并设为仅所有者可读（600）"

  write_unit
  info "已注册 systemd 服务：${UNIT}"

  step "启动前自检"
  smoke_test || die "程序自检失败，已终止安装。以上为程序输出。"
  info "自检通过：配置可读、数据库可写、端口可监听"

  step "启动服务"
  svc_start || { service_fail_hint; die "服务启动失败"; }
  health_check || warn "服务已启动，但 HTTP 探活未通过，请稍后查看日志"

  print_summary
}

print_summary() {
  local ip
  ip="$(hostname -I 2>/dev/null | awk '{print $1}')"
  [ -n "$ip" ] || ip="<服务器IP>"
  local ver; ver="$(installed_version 2>/dev/null || echo "?")"
  step "完成"
  printf '  版本       : v%s\n' "$ver"
  printf '  访问地址   : http://%s:%s\n' "$ip" "$(json_num web_port)"
  printf '  登录账号   : %s\n' "$(json_str web_user)"
  printf '  登录密码   : %s\n' "$(json_str web_pass)"
  printf '  配置文件   : %s（权限 600，内含明文密码）\n' "$CONF"
  printf '  数据目录   : %s\n' "$DATA"
  printf '\n  下次管理：bash install.sh manage\n'
  printf '  或直接执行：systemctl status %s / journalctl -u %s -f\n' "$APP_NAME" "$APP_NAME"
}

# ---------------- 升级 ----------------
cmd_upgrade() {
  printf '%s服务器监控 · 升级%s\n' "$C_B" "$C_0"
  reexec_root
  preflight
  installed || die "尚未安装。请先执行：install.sh install"

  local cur new tmpdir
  cur="$(installed_version || true)"
  [ -n "$cur" ] || cur="0.0.0"
  info "当前版本：v${cur}"
  new="$(resolve_latest "${OPT_VERSION:-}")"

  if [ "$new" = "$cur" ]; then
    info "已是最新版本 v${new}，无需升级"
    return 0
  fi
  if ! ver_gt "$new" "$cur"; then
    warn "目标版本 v${new} 不比当前版本 v${cur} 新"
    confirm "仍要降级/重装？[y/N]" "N" || return 0
  fi

  local backup_conf="" bin_backup="${BIN}.prev"
  step "备份当前程序与配置"
  cp -p "$BIN" "$bin_backup" || die "备份当前程序失败，已中止"
  info "程序备份：${bin_backup}"
  if [ -f "$CONF" ]; then
    backup_conf="${CONF}.bak.$(date +%Y%m%d%H%M%S)"
    cp -p "$CONF" "$backup_conf" && info "配置备份：${backup_conf}"
  fi

  tmpdir="$(mktemp -d "${TMPDIR:-/tmp}/smd-dl.XXXXXX")"
  # shellcheck disable=SC2064
  trap "rm -rf '$tmpdir'" EXIT
  acquire_binary "$tmpdir"

  step "停止服务"
  systemctl stop "$APP_NAME" 2>/dev/null || true

  step "替换程序"
  if ! install -m 0755 "$BIN_SRC" "$BIN"; then
    systemctl start "$APP_NAME" 2>/dev/null || true
    die "替换程序失败，服务已按原版本重新启动"
  fi
  info "已更新到 v${new}"

  # 配置与数据保持原样：init 不再调用，避免覆盖已有账号、端口、Key。
  write_unit
  smoke_test || {
    warn "新版本自检失败，开始回滚"
    install -m 0755 "$bin_backup" "$BIN"
    systemctl start "$APP_NAME" 2>/dev/null || true
    die "升级失败，已回滚到 v${cur}。以上为程序输出。"
  }

  if ! svc_start || ! health_check; then
    warn "新版本启动或探活失败，开始回滚"
    systemctl stop "$APP_NAME" 2>/dev/null || true
    install -m 0755 "$bin_backup" "$BIN"
    systemctl start "$APP_NAME" 2>/dev/null || true
    service_fail_hint
    die "升级失败，已回滚到 v${cur}"
  fi

  step "升级完成"
  printf '  %s -> %s\n' "v${cur}" "v${new}"
  [ -n "$backup_conf" ] && printf '  配置备份 : %s\n' "$backup_conf"
  printf '  程序备份 : %s（确认无误后可删除）\n' "$bin_backup"
}

# ---------------- 状态 ----------------
mask_key() { [ -n "$1" ] && printf '%s' "${1:0:4}****${1: -4}" || printf '未生成'; }

cmd_status() {
  if ! installed; then
    printf '%s未安装%s（期望路径 %s）\n' "$C_Y" "$C_0" "$BIN"
    return 2
  fi
  local port
  port="$(json_num web_port)"
  printf '%s服务器监控%s  v%s\n' "$C_B" "$C_0" "$(installed_version 2>/dev/null || echo '?')"
  printf '  服务状态 : %s\n' "$(svc_state)"
  printf '  开机自启 : %s\n' "$(svc_enabled)"
  printf '  Web 端口 : %s\n' "${port:-?}"
  printf '  登录账号 : %s\n' "$(json_str web_user)"
  printf '  数据接收 : %s（端口 %s，Key %s）\n' \
    "$(json_bool receive_enabled)" "$(json_num receive_port)" "$(mask_key "$(json_str receive_api_key)")"
  printf '  查询 API : %s（Key %s）\n' \
    "$(json_bool api_enabled)" "$(mask_key "$(json_str api_key)")"
  printf '  数据库   : %s\n' "$(du -sh "$DATA" 2>/dev/null | awk '{print $1}' || echo '-')"
  printf '  启动时间 : %s\n' "$(systemctl show -p ActiveEnterTimestamp --value "$APP_NAME" 2>/dev/null || echo '-')"
}

# ---------------- 管理菜单 ----------------
change_port() {
  local old new
  old="$(json_num web_port)"
  new="$(ask "新的 Web 端口 [${old}]: " "$old")"
  case "$new" in ''|*[!0-9]*) die "端口必须是数字" ;; esac
  [ "$new" -ge 1 ] && [ "$new" -le 65535 ] || die "端口需在 1~65535 之间"
  [ "$new" != "$old" ] || { info "端口未变化"; return 0; }
  if port_in_use "$new" && ! port_owner_is_self "$new"; then
    warn "端口 ${new} 已被占用："; port_show "$new"
    confirm "仍要改用该端口？[y/N]" "N" || return 0
  fi
  # 不传 --web-user/--web-pass：init 会沿用配置中已有的账号与口令。
  "$BIN" init --config "$CONF" --data-dir "$DATA" --web-port "$new" >/dev/null \
    || die "写入配置失败"
  systemctl restart "$APP_NAME" || { service_fail_hint; die "重启失败"; }
  health_check || warn "端口已改，但 HTTP 探活未通过，请检查日志"
  info "Web 端口已改为 ${new}，服务已重启"
}

change_password() {
  local user pass
  user="$(json_str web_user)"; [ -n "$user" ] || user="$DEFAULT_USER"
  user="$(ask "登录账号 [${user}]: " "$user")"
  while :; do
    pass="$(ask_secret "新密码（至少 8 位，回车随机生成）: ")"
    if [ -z "$pass" ]; then pass="$(gen_password)"; info "已随机生成密码"; break; fi
    check_password "$user" "$pass" && break
    warn "请重新输入"
  done
  "$BIN" init --config "$CONF" --data-dir "$DATA" --web-user "$user" --web-pass "$pass" >/dev/null \
    || die "写入配置失败"
  systemctl restart "$APP_NAME" >/dev/null 2>&1 || true
  info "账号已更新，服务已重启（旧登录态立即失效）"
  printf '  登录账号 : %s\n  登录密码 : %s\n' "$user" "$pass"
}

show_config() {
  printf '  配置文件 : %s\n' "$CONF"
  [ -f "$CONF" ] || { warn "配置文件不存在"; return 0; }
  printf '  本机识别码: %s\n' "$(json_str uuid)"
  printf '  服务器名称: %s\n' "$(json_str server_name)"
  printf '  Web 端口 : %s\n' "$(json_num web_port)"
  printf '  登录账号 : %s\n' "$(json_str web_user)"
  printf '  登录密码 : %s\n' "$(json_str web_pass)"
  printf '  数据接收 : %s  端口 %s  接收 Key %s\n' \
    "$(json_bool receive_enabled)" "$(json_num receive_port)" "$(json_str receive_api_key)"
  printf '  查询 API : %s  查询 Key %s\n' \
    "$(json_bool api_enabled)" "$(json_str api_key)"
  printf '  数据上报 : %s  目标 %s:%s  间隔 %ss\n' \
    "$(json_bool report_enabled)" "$(json_str report_target_ip)" \
    "$(json_num report_target_port)" "$(json_num report_interval)"
}

cmd_diag() {
  step "系统诊断"
  printf '程序版本 : v%s\n' "$(installed_version 2>/dev/null || echo '未安装')"
  printf '二进制   : %s\n' "$BIN"
  [ -x "$BIN" ] && ls -l "$BIN"
  printf '服务状态 : %s / %s\n' \
    "$(svc_state)" \
    "$(svc_enabled)"
  printf '配置文件 : %s\n' "$CONF"
  [ -f "$CONF" ] && ls -l "$CONF"
  local port; port="$(json_num web_port)"
  if [ -n "$port" ]; then
    printf '端口 %s : %s\n' "$port" "$(port_in_use "$port" && echo 已监听 || echo 未监听)"
  fi
  printf '磁盘     : %s\n' "$(df -h "$APP_DIR" 2>/dev/null | awk 'NR==2{print $4" 可用（"$5" 已用）"}')"
  printf '\n最近 30 行日志：\n'
  journalctl -u "$APP_NAME" -n 30 --no-pager 2>/dev/null || true
}

cmd_uninstall() {
  reexec_root
  installed || { warn "未检测到安装：${BIN}"; return 0; }
  printf '%s即将卸载服务器监控 v%s%s\n' "$C_Y" "$(installed_version 2>/dev/null || echo '?')" "$C_0"
  printf '  程序     : %s\n' "$BIN"
  printf '  服务     : %s\n' "$APP_NAME"
  # --yes 视为「确认卸载」，但删除数据必须单独再确认两次，不受 --yes 影响。
  [ "$NONSTOP" = 1 ] || confirm "确认卸载（配置与数据默认保留）？[y/N]" "N" || { info "已取消"; return 0; }

  systemctl stop "$APP_NAME" 2>/dev/null || true
  systemctl disable "$APP_NAME" >/dev/null 2>&1 || true
  rm -f "$UNIT"
  systemctl daemon-reload 2>/dev/null || true
  rm -f "$BIN" "$BIN.prev"
  info "程序与服务已移除"

  if [ -f "$CONF" ] || [ -d "$DATA" ]; then
    printf '\n以下内容仍然保留（内含账号密码与全部历史数据）：\n'
    printf '  配置文件 : %s\n' "$CONF"
    printf '  数据目录 : %s（%s）\n' "$DATA" "$(du -sh "$DATA" 2>/dev/null | awk '{print $1}')"
    if confirm "是否一并删除配置与全部历史数据？此操作不可恢复 [y/N]" "N"; then
      printf '%s警告：将永久删除 %s 与 %s%s\n' "$C_R" "$CONF" "$DATA" "$C_0"
      if confirm "再次确认删除？[y/N]" "N"; then
        rm -rf "$CONF" "$DATA"
        rmdir "$APP_DIR" 2>/dev/null || true
        info "配置与数据已删除"
      else
        info "已保留"
      fi
    else
      info "已保留配置与数据，重新安装时会自动沿用"
    fi
  fi
}

cmd_manage() {
  reexec_root
  while :; do
    clear 2>/dev/null || true
    printf '%s┌──────────────────────────────────────────┐%s\n' "$C_B" "$C_0"
    printf '%s│%s  服务器监控 · 管理                     %s│%s\n' "$C_B" "$C_0" "$C_B" "$C_0"
    printf '%s└──────────────────────────────────────────┘%s\n' "$C_B" "$C_0"
    if installed; then
      printf '  当前：v%s  服务 %s  端口 %s\n\n' \
        "$(installed_version 2>/dev/null || echo '?')" \
        "$(svc_state)" \
        "$(json_num web_port)"
    else
      printf '  %s当前未安装%s\n\n' "$C_Y" "$C_0"
    fi
    cat <<'MENU'
  1) 查看运行状态
  2) 启动服务
  3) 停止服务
  4) 重启服务
  5) 查看实时日志
  6) 修改 Web 端口
  7) 重置登录密码
  8) 查看完整配置（含 Key）
  9) 系统诊断
  u) 卸载
  0) 返回
MENU
    printf '\n请选择: '
    case "$(_readline)" in
      1) cmd_status; pause ;;
      2) systemctl start "$APP_NAME" && info "已启动" || warn "启动失败，请查看日志"; pause ;;
      3) systemctl stop "$APP_NAME" && info "已停止" || warn "停止失败"; pause ;;
      4) systemctl restart "$APP_NAME" && info "已重启" || warn "重启失败"; pause ;;
      5)
        installed || { warn "未安装"; pause; continue; }
        printf '%s按 Ctrl-C 退出日志%s\n' "$C_D" "$C_0"
        journalctl -u "$APP_NAME" -n 100 -f
        ;;
      6) installed || { warn "未安装"; pause; continue; }
         change_port; pause ;;
      7) installed || { warn "未安装"; pause; continue; }
         change_password; pause ;;
      8) show_config; pause ;;
      9) cmd_diag; pause ;;
      u|U) cmd_uninstall; pause ;;
      0|"") return 0 ;;
      *) ;;
    esac
  done
}

main_menu() {
  reexec_root
  while :; do
    clear 2>/dev/null || true
    printf '%s┌──────────────────────────────────────────┐%s\n' "$C_B" "$C_0"
    printf '%s│%s  服务器监控 · 安装向导                 %s│%s\n' "$C_B" "$C_0" "$C_B" "$C_0"
    printf '%s└──────────────────────────────────────────┘%s\n' "$C_B" "$C_0"
    if installed; then
      printf '  已安装：v%s  服务 %s\n\n' \
        "$(installed_version 2>/dev/null || echo '?')" \
        "$(svc_state)"
    else
      printf '  %s未安装%s\n\n' "$C_Y" "$C_0"
    fi
    cat <<'MENU'
  1) 全新安装
  2) 升级到最新版本（保留配置与数据）
  3) 管理（启停 / 日志 / 改端口 / 改密码 / 诊断 / 卸载）
  4) 查看运行状态
  0) 退出
MENU
    printf '\n请选择: '
    case "$(_readline)" in
      1) cmd_install; pause ;;
      2) cmd_upgrade; pause ;;
      3) cmd_manage ;;
      4) cmd_status; pause ;;
      0|"") return 0 ;;
      *) ;;
    esac
  done
}

usage() {
  cat <<EOF
服务器监控 · 安装 / 管理脚本

用法：
  install.sh               交互菜单（推荐）
  install.sh install       全新安装
  install.sh upgrade       升级（保留配置与数据）
  install.sh manage        管理菜单
  install.sh status        查看状态
  install.sh uninstall     卸载

install / upgrade 参数：
  --port N       Web 端口        --user U       登录账号
  --pass P       登录密码        --bin PATH     使用本地二进制（离线）
  --version V    指定版本        --yes          使用默认值，不再确认

示例：
  # 一行安装（推荐，普通用户执行，脚本内部会自己调 sudo）
  bash <(curl -fsSL https://raw.githubusercontent.com/${REPO}/main/install.sh)

  # 国内网络 + 静默安装
  SMD_MIRROR=https://ghfast.top/ bash <(curl -fsSL https://raw.githubusercontent.com/${REPO}/main/install.sh) install --yes --port 8080

  # 先下载再执行（需要 sudo 时用这种，或 curl ... | sudo bash）
  curl -fsSL https://raw.githubusercontent.com/${REPO}/main/install.sh -o install.sh
  sudo bash install.sh

  # 离线：先上传二进制，再执行
  bash install.sh install --bin ./server-monitor-linux-amd64

注意：
  不要写成 "sudo bash <(curl ...)"。sudo 默认会关闭继承的文件描述符，
  bash 在新进程里打不开 /dev/fd/63，会报：
      bash: /dev/fd/63: No such file or directory
      curl: (23) Failure writing output to destination
  这不是网络或脚本的问题，换成上面的写法即可。本脚本检测到非 root 时会自动提权，
  因此正常情况下不需要手动加 sudo。
EOF
}

# ---------------- 参数解析 ----------------
parse_args() {
  local a
  while [ $# -gt 0 ]; do
    a="$1"
    case "$a" in
      install|upgrade|manage|status|uninstall|version|help|-h|--help) CMD="$a" ;;
      --port)    OPT_PORT="${2:-}"; shift ;;
      --user)    OPT_USER="${2:-}"; shift ;;
      --pass)    OPT_PASS="${2:-}"; shift ;;
      --bin)     OPT_BIN="${2:-}"; shift ;;
      --version) OPT_VERSION="${2:-}"; shift ;;
      -y|--yes)  NONSTOP=1 ;;
      *) warn "忽略未知参数：${a}" ;;
    esac
    shift
  done
}

main() {
  # 先存一份原始参数：非 root 时会用 sudo 重新执行自身。
  # 这里用「先赋值空数组再按需填充」而不是 main 内 local 数组，
  # 是为了兼容 bash 3.2（macOS 自带）下 set -u 对空数组展开的严格检查。
  parse_args "$@"
  [ $# -gt 0 ] && ARGS=("$@")
  case "${CMD:-}" in
    ""|help|-h|--help)
      if [ "${CMD:-}" = "" ] && [ "$INTERACTIVE" = 1 ]; then main_menu; else usage; fi
      ;;
    install)   cmd_install ;;
    upgrade)   cmd_upgrade ;;
    manage)    cmd_manage ;;
    status)    cmd_status; exit $? ;;
    uninstall) cmd_uninstall ;;
    version)   printf 'install.sh (repo %s)\n' "$REPO" ;;
  esac
}

main "$@"

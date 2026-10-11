#!/usr/bin/env bash
#
# 服务器监控 — 部署初始化脚本
#
# 只负责初始化，不参与程序运行时逻辑。不使用命令行参数，全部信息通过交互式录入。
# 允许以 root 账户或 sudo 组账户运行。
#
# 交互式录入项（全部直接回车使用默认值）：
#   Web 登录账号（默认 smdadmin）、Web 登录密码（默认随机生成）、
#   待部署的二进制文件路径、Web 监听端口（默认沿用已有配置或 8080）。
#
# 执行流程：
#   权限检查 -> 环境检查 -> 创建安装目录 -> 录入信息（账号 / 密码 / 二进制 / 端口）
#   -> 停止旧服务 -> 安装二进制 -> 写入配置（调用 program init）
#   -> 写入 systemd 单元 -> 启动前自检 -> 启动服务 -> 输出部署信息
#
# 路径约定：程序二进制、配置文件、数据目录统一位于默认安装目录 /opt/server-monitor/ 下。
#
# 服务异常时（启动失败、反复重启）可运行 deploy/diagnose.sh 定位原因。

set -euo pipefail

# ---------------- 固定路径约定 ----------------
# 程序二进制、配置文件、数据目录全部统一放在默认安装目录下。
APP_NAME="server-monitor"
APP_DIR="/opt/server-monitor"
BIN_DEST="${APP_DIR}/${APP_NAME}"
CONFIG_FILE="${APP_DIR}/config.json"
DATA_DIR="${APP_DIR}/data"
# systemd 单元必须位于 /etc/systemd/system/，由 systemd 规定，无法并入安装目录。
UNIT_FILE="/etc/systemd/system/${APP_NAME}.service"

# 早期版本使用的分散目录，仅用于升级时继承已有配置与历史数据。
LEGACY_CONFIG_FILE="/etc/server-monitor/config.json"
LEGACY_DATA_DIR="/var/lib/server-monitor"

DEFAULT_USER="smdadmin"
DEFAULT_WEB_PORT="8080"

SUDO=""

# ---------------- 输出辅助 ----------------
info()  { printf '\033[32m[信息]\033[0m %s\n' "$*"; }
warn()  { printf '\033[33m[警告]\033[0m %s\n' "$*"; }
step()  { printf '\n\033[36m==> %s\033[0m\n' "$*"; }
die()   { printf '\033[31m[错误]\033[0m %s\n' "$*" >&2; exit 1; }

# ---------------- 1. 权限检查 ----------------
check_privilege() {
  step "权限检查"

  if [ "$(id -u)" -eq 0 ]; then
    SUDO=""
    info "当前用户为 root"
    return 0
  fi

  if id -nG 2>/dev/null | tr ' ' '\n' | grep -qx 'sudo' \
     || id -nG 2>/dev/null | tr ' ' '\n' | grep -qx 'wheel'; then
    SUDO="sudo"
    info "当前用户属于 sudo 组，提权操作将通过 sudo 执行"
    return 0
  fi

  die "当前用户既不是 root 也不属于 sudo 组，无法完成部署。请使用 root 或 sudo 组账户运行本脚本。"
}

# ---------------- 2. 环境检查 ----------------
check_env() {
  step "环境检查"

  if [ ! -d /run/systemd/system ] || ! command -v systemctl >/dev/null 2>&1; then
    die "当前系统不支持 systemd（未检测到 systemctl 或 /run/systemd/system），无法部署。"
  fi
  info "systemd 可用"

  if [ ! -t 0 ]; then
    die "本脚本需要交互式录入信息，请在交互式终端中运行。"
  fi

  if ! $SUDO true 2>/dev/null; then
    die "无法获取管理员权限，请确认当前账户可以使用 sudo。"
  fi
}

# ---------------- 3. 录入信息 ----------------
gen_password() {
  # 使用 /dev/urandom 生成 16 位字母数字密码（十六进制，无转义风险）
  head -c 12 /dev/urandom | od -An -tx1 | tr -d ' \n' | cut -c1-16
}

# 校验 Web 登录密码强度。策略与 Go 侧 config.ValidatePassword 保持一致。
# 这里先拦一道，是为了在录入阶段就提示重填，而不是等 init 失败后终止整个部署。
# 两处若有出入，以 Go 侧为准（它才是最终写入配置的那一方）。
check_password_strength() {
  local user="$1" pass="$2" lower first

  if [ "${#pass}" -lt 8 ]; then
    warn "密码至少需要 8 个字符（当前 ${#pass} 个）"
    return 1
  fi
  if [ "${#pass}" -gt 128 ]; then
    warn "密码不能超过 128 个字符"
    return 1
  fi

  lower="$(printf '%s' "$pass" | tr '[:upper:]' '[:lower:]')"
  case "$lower" in
    password|passw0rd|password1|12345678|123456789|1234567890|123456|qwertyui|qwerty123|iloveyou|letmein1|admin123|administrator|changeme|welcome1|smdadmin|servermonitor|monitor123|abc12345|11111111|00000000|88888888)
      warn "该密码是常见弱口令，请更换一个"
      return 1
      ;;
  esac

  if [ -n "$user" ] && [ "$lower" = "$(printf '%s' "$user" | tr '[:upper:]' '[:lower:]')" ]; then
    warn "密码不能与登录账号相同"
    return 1
  fi

  # 全部为同一字符（如 88888888）；引号确保首字符为 * 或 ? 时按字面处理。
  first="${pass:0:1}"
  if [ -z "${pass//"$first"/}" ]; then
    warn "密码不能是同一个字符的重复"
    return 1
  fi

  return 0
}

# 从已有配置文件中读取一个数值字段，不存在时输出空。
read_cfg_num() {
  local key="$1"
  [ -f "$CONFIG_FILE" ] || return 0
  sed -n "s/.*\"${key}\"[[:space:]]*:[[:space:]]*\([0-9]\{1,5\}\).*/\1/p" "$CONFIG_FILE" 2>/dev/null | head -n1 || true
}

# 判断端口是否已被监听。
# 注意：这里刻意不用 grep -q —— 在 set -o pipefail 下，grep -q 提前退出会让上游 sed/ss
# 收到 SIGPIPE 而返回非 0，导致"端口其实被占用却判定为空闲"。用 awk 自行结束管道最稳妥。
port_in_use() {
  local port="$1"
  if command -v ss >/dev/null 2>&1; then
    $SUDO ss -lnt 2>/dev/null | awk -v p=":${port}" 'NR>1 && index($4, p) { found=1 } END { exit(found ? 0 : 1) }'
  elif command -v lsof >/dev/null 2>&1; then
    $SUDO lsof -nP -iTCP:"${port}" -sTCP:LISTEN >/dev/null 2>&1
  else
    return 1
  fi
}

# 判断占用端口的进程是否就是本程序（升级部署时属于正常占用，可忽略）。
port_owner_is_self() {
  local port="$1"
  command -v ss >/dev/null 2>&1 || return 1
  $SUDO ss -lntp 2>/dev/null | awk -v p=":${port}" -v n="${APP_NAME}" 'NR>1 && index($4, p) && index($0, n) { found=1 } END { exit(found ? 0 : 1) }'
}

# 打印占用端口的进程信息（便于用户判断是什么程序占用）。
port_show() {
  local port="$1"
  if command -v ss >/dev/null 2>&1; then
    $SUDO ss -lntp 2>/dev/null | awk -v p=":${port}" 'NR>1 && index($4, p)' | sed 's/^/    /'
  elif command -v lsof >/dev/null 2>&1; then
    $SUDO lsof -nP -iTCP:"${port}" -sTCP:LISTEN 2>/dev/null | sed 's/^/    /'
  fi
  return 0
}

# 录入 Web 监听端口：默认沿用已有配置或 8080，并校验取值范围、与接收端口冲突、是否被占用。
read_web_port() {
  local default_port prev_port recv_port answer
  default_port="$DEFAULT_WEB_PORT"

  prev_port="$(read_cfg_num web_port)"
  if [ -n "$prev_port" ]; then
    default_port="$prev_port"
    info "检测到已有配置文件，默认沿用 Web 端口 ${default_port}"
  fi
  recv_port="$(read_cfg_num receive_port)"

  while :; do
    read -r -p "Web 监听端口 [${default_port}]: " WEB_PORT || true
    WEB_PORT="${WEB_PORT:-$default_port}"

    # 纯 bash 判断，避免管道与子进程带来的边界问题
    case "$WEB_PORT" in
      ''|*[!0-9]*)
        warn "端口必须是数字"
        continue
        ;;
    esac
    if [ "$WEB_PORT" -lt 1 ] || [ "$WEB_PORT" -gt 65535 ]; then
      warn "端口需在 1~65535 之间"
      continue
    fi
    if [ "$WEB_PORT" -lt 1024 ]; then
      warn "端口 ${WEB_PORT} 属于特权端口（<1024）：服务以 root 运行可用，以普通用户运行会失败"
    fi
    if [ -n "$recv_port" ] && [ "$WEB_PORT" = "$recv_port" ]; then
      warn "该端口与数据接收端口 ${recv_port} 相同，请更换"
      continue
    fi
    if port_in_use "$WEB_PORT"; then
      if port_owner_is_self "$WEB_PORT"; then
        info "端口 ${WEB_PORT} 当前由本程序旧实例占用，部署过程中会自动停止它"
        break
      fi
      warn "端口 ${WEB_PORT} 已被其它程序占用："
      port_show "$WEB_PORT"
      read -r -p "仍要使用该端口？[y/N]: " answer || true
      case "${answer:-N}" in
        [yY]|[yY][eE][sS]) break ;;
        *) continue ;;
      esac
    fi
    break
  done
}

read_inputs() {
  step "录入部署信息（直接回车使用默认值）"

  read -r -p "Web 登录账号 [${DEFAULT_USER}]: " WEB_USER || true
  WEB_USER="${WEB_USER:-$DEFAULT_USER}"
  [ -n "$WEB_USER" ] || die "Web 登录账号不能为空"

  local auto_pass
  auto_pass="$(gen_password)"
  while :; do
    read -r -p "Web 登录密码（至少 8 位，回车随机生成）: " WEB_PASS || true
    if [ -z "${WEB_PASS:-}" ]; then
      WEB_PASS="$auto_pass"
      info "已随机生成密码"
      break
    fi
    if check_password_strength "$WEB_USER" "$WEB_PASS"; then
      break
    fi
    warn "请重新输入，或直接回车使用随机生成的强密码"
  done

  while :; do
    read -r -p "待部署的程序二进制文件路径: " BIN_SRC || true
    if [ -z "${BIN_SRC:-}" ]; then
      warn "二进制文件路径不能为空"
      continue
    fi
    if [ ! -e "$BIN_SRC" ]; then
      warn "文件不存在：$BIN_SRC"
      continue
    fi
    if [ ! -f "$BIN_SRC" ]; then
      warn "不是普通文件：$BIN_SRC"
      continue
    fi
    BIN_SRC="$(cd "$(dirname "$BIN_SRC")" && pwd)/$(basename "$BIN_SRC")"
    break
  done

  read_web_port

  printf '\n请确认部署信息：\n'
  printf '  二进制来源 : %s\n' "$BIN_SRC"
  printf '  安装目录   : %s\n' "$APP_DIR"
  printf '  程序二进制 : %s\n' "$BIN_DEST"
  printf '  配置文件   : %s\n' "$CONFIG_FILE"
  printf '  数据目录   : %s\n' "$DATA_DIR"
  printf '  Web 端口   : %s\n' "$WEB_PORT"
  printf '  登录账号   : %s\n' "$WEB_USER"
  printf '\n'
  read -r -p "确认开始部署？[Y/n]: " CONFIRM || true
  case "${CONFIRM:-Y}" in
    [yY]|[yY][eE][sS]|"") ;;
    *) die "已取消部署" ;;
  esac
}

# ---------------- 4. 创建目录 ----------------
prepare_dirs() {
  step "创建安装目录"
  $SUDO mkdir -p "$APP_DIR" "$DATA_DIR"
  $SUDO chmod 755 "$APP_DIR" "$DATA_DIR"
  info "安装目录：$APP_DIR"
  info "配置文件：$CONFIG_FILE"
  info "数据目录：$DATA_DIR"

  # 兼容早期版本的分散目录：仅当新位置为空时才继承，避免覆盖现有数据。
  if [ "$LEGACY_CONFIG_FILE" != "$CONFIG_FILE" ] \
     && [ -f "$LEGACY_CONFIG_FILE" ] && [ ! -f "$CONFIG_FILE" ]; then
    $SUDO cp -p "$LEGACY_CONFIG_FILE" "$CONFIG_FILE"
    info "已从 $LEGACY_CONFIG_FILE 继承原有配置"
  fi

  if [ "$LEGACY_DATA_DIR" != "$DATA_DIR" ] \
     && [ -d "$LEGACY_DATA_DIR" ] && [ -z "$(ls -A "$DATA_DIR" 2>/dev/null)" ]; then
    if $SUDO cp -a "$LEGACY_DATA_DIR"/. "$DATA_DIR"/; then
      info "已从 $LEGACY_DATA_DIR 继承历史数据库"
    else
      die "继承历史数据失败，已终止部署以避免数据丢失。请手动迁移 $LEGACY_DATA_DIR 到 $DATA_DIR 后重试。"
    fi
  fi
}

# ---------------- 5. 停止旧服务 ----------------
stop_old_service() {
  step "停止旧服务（配置与数据库保留）"
  if command -v systemctl >/dev/null 2>&1; then
    $SUDO systemctl stop "$APP_NAME" 2>/dev/null || true
    info "旧服务已停止（如原先未运行则忽略）"
  fi
}

# ---------------- 6. 安装二进制 ----------------
install_binary() {
  step "安装二进制"
  $SUDO install -m 0755 "$BIN_SRC" "$BIN_DEST"
  info "已安装到 $BIN_DEST"
}

# ---------------- 7. 写入配置 ----------------
write_config() {
  step "写入配置（调用程序 init 子命令）"
  local init_args=(
    init
    --config "$CONFIG_FILE"
    --data-dir "$DATA_DIR"
    --web-user "$WEB_USER"
    --web-pass "$WEB_PASS"
    --web-port "$WEB_PORT"
  )
  if ! $SUDO "$BIN_DEST" "${init_args[@]}"; then
    die "初始化配置失败，部署终止。"
  fi
  $SUDO chmod 600 "$CONFIG_FILE"
  info "配置已写入并限制为仅所有者可读（600）"
}

# ---------------- 8. 写入 systemd 单元 ----------------
write_unit() {
  step "写入 systemd 单元"
  $SUDO tee "$UNIT_FILE" >/dev/null <<EOF
[Unit]
Description=Server Monitor
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=${BIN_DEST} --config ${CONFIG_FILE} --data-dir ${DATA_DIR}
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
  $SUDO chmod 644 "$UNIT_FILE"
  info "已写入 $UNIT_FILE"
}

# ---------------- 9. 启动前自检 ----------------
# 以服务身份在前台试跑几秒：配置能否读取、数据库能否写入、Web 端口能否监听，
# 任何一步失败都会立刻打印程序自己的错误信息，避免服务进入无声的崩溃重启循环。
smoke_test() {
  step "启动前自检（以服务身份试运行）"

  if ! command -v timeout >/dev/null 2>&1; then
    warn "未找到 timeout 命令，跳过启动前自检"
    return 0
  fi

  local log rc=0
  log="/tmp/.${APP_NAME}-smoke.$$"
  $SUDO timeout 3 "$BIN_DEST" --config "$CONFIG_FILE" --data-dir "$DATA_DIR" >"$log" 2>&1 || rc=$?

  local out=""
  out="$(cat "$log" 2>/dev/null || true)"
  rm -f "$log"

  # 正常启动会由 timeout 以 124 结束；其它非 0 退出码说明程序自身启动失败。
  if [ "$rc" -ne 0 ] && [ "$rc" -ne 124 ] && [ "$rc" -ne 143 ]; then
    printf '%s\n' "$out" >&2
    die "程序自检失败（退出码 ${rc}），已终止部署。以上为程序输出，请据此排查。"
  fi
  if printf '%s' "$out" | grep -qE '启动失败|服务异常退出'; then
    printf '%s\n' "$out" >&2
    die "程序自检失败，已终止部署。以上为程序输出，请据此排查。"
  fi

  # 等待 SIGTERM 后的优雅退出真正释放端口，避免影响随后 systemd 启动的服务。
  sleep 1
  info "自检通过：配置可读、数据库可写、Web 端口可监听"
}

# ---------------- 10. 启动服务 ----------------
start_service() {
  step "启动服务"
  $SUDO systemctl daemon-reload
  $SUDO systemctl enable "$APP_NAME" >/dev/null 2>&1 || die "设置开机自启失败。"
  if ! $SUDO systemctl restart "$APP_NAME"; then
    $SUDO journalctl -u "$APP_NAME" -n 30 --no-pager >&2 || true
    die "启动服务失败，请执行 journalctl -u ${APP_NAME} -n 50 查看原因。"
  fi

  local i
  for i in $(seq 1 15); do
    if $SUDO systemctl is-active --quiet "$APP_NAME"; then
      info "服务已启动并处于运行状态"
      return 0
    fi
    sleep 1
  done

  $SUDO systemctl status "$APP_NAME" --no-pager -l >&2 || true
  $SUDO journalctl -u "$APP_NAME" -n 30 --no-pager >&2 || true
  die "服务启动后未进入运行状态，部署失败。请执行 journalctl -u ${APP_NAME} -n 50 查看原因。"
}

# ---------------- 10. 输出部署信息 ----------------
print_summary() {
  local ip addr
  ip="$(hostname -I 2>/dev/null | awk '{print $1}')" || ip=""
  if [ -z "$ip" ]; then
    ip="$(ip route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="src"){print $(i+1); exit}}')" || ip=""
  fi
  [ -n "$ip" ] || ip="<服务器IP>"

  step "部署完成"
  printf '  访问地址   : http://%s:%s\n' "$ip" "$WEB_PORT"
  printf '  登录账号   : %s\n' "$WEB_USER"
  printf '  登录密码   : %s\n' "$WEB_PASS"
  printf '\n'
  printf '  安装目录   : %s\n' "$APP_DIR"
  printf '  程序二进制 : %s\n' "$BIN_DEST"
  printf '  配置文件   : %s\n' "$CONFIG_FILE"
  printf '  数据目录   : %s\n' "$DATA_DIR"
  printf '  systemd    : %s\n' "$UNIT_FILE"
  printf '\n'
  printf '常用管理命令：\n'
  printf '  查看状态   : systemctl status %s\n' "$APP_NAME"
  printf '  重启服务   : systemctl restart %s\n' "$APP_NAME"
  printf '  停止服务   : systemctl stop %s\n' "$APP_NAME"
  printf '  查看日志   : journalctl -u %s -n 100 -f\n' "$APP_NAME"
  printf '\n'
  warn "登录账号与密码已明文保存在 ${CONFIG_FILE} 中（权限 600），请妥善保管。"
  warn "如需修改账号密码，可直接编辑该配置文件后重启服务。"
}

main() {
  printf '\033[36m服务器监控部署脚本\033[0m\n'
  check_privilege
  check_env
  prepare_dirs
  read_inputs
  stop_old_service
  install_binary
  write_config
  write_unit
  smoke_test
  start_service
  print_summary
}

main "$@"

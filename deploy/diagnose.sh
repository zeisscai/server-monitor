#!/usr/bin/env bash
#
# 服务器监控 — 故障诊断脚本
#
# 用途：服务启动失败、反复重启（activating (auto-restart)）时，一次性给出完整原因。
# 用法：sudo bash diagnose.sh
#
# 脚本只读，不修改任何文件；唯一会做的"动作"是在前台试运行程序 3 秒以捕获错误输出。

set -uo pipefail

APP_NAME="server-monitor"
APP_DIR="/opt/server-monitor"
BIN_DEST="${APP_DIR}/${APP_NAME}"
CONFIG_FILE="${APP_DIR}/config.json"
DATA_DIR="${APP_DIR}/data"
UNIT_FILE="/etc/systemd/system/${APP_NAME}.service"

SUDO=""
[ "$(id -u)" -eq 0 ] || SUDO="sudo"

hr() { printf '\n\033[36m===== %s =====\033[0m\n' "$*"; }
ok() { printf '  \033[32m[正常]\033[0m %s\n' "$*"; }
bad() { printf '  \033[31m[异常]\033[0m %s\n' "$*"; }
warn() { printf '  \033[33m[注意]\033[0m %s\n' "$*"; }
info() { printf '  %s\n' "$*"; }

hr "0. 服务状态"
$SUDO systemctl is-active "$APP_NAME" 2>&1 | sed 's/^/  is-active: /'
$SUDO systemctl is-enabled "$APP_NAME" 2>&1 | sed 's/^/  is-enabled: /'

hr "1. systemd 单元"
if [ -f "$UNIT_FILE" ]; then
  ok "存在：$UNIT_FILE"
  sed 's/^/  | /' "$UNIT_FILE"
else
  bad "不存在：$UNIT_FILE（未部署或路径不同）"
fi

hr "2. 文件与权限"
for p in "$APP_DIR" "$BIN_DEST" "$CONFIG_FILE" "$DATA_DIR"; do
  if [ -e "$p" ]; then
    info "$($SUDO ls -ld "$p" 2>/dev/null | awk '{print $1, $3":"$4, $NF}')"
  else
    bad "缺失：$p"
  fi
done

# 服务实际以哪个用户运行（单元里没有 User= 即 root）
svc_user="$(sed -n 's/^[[:space:]]*User[[:space:]]*=[[:space:]]*\(.*\)$/\1/p' "$UNIT_FILE" 2>/dev/null | tail -n1)"
[ -n "$svc_user" ] || svc_user="root"
hr "3. 服务运行身份：${svc_user}"
if id "$svc_user" >/dev/null 2>&1; then
  if $SUDO -u "$svc_user" test -r "$CONFIG_FILE" 2>/dev/null; then
    ok "可读取配置文件"
  else
    bad "无法读取 $CONFIG_FILE —— 服务会以「启动失败：读取配置文件失败 permission denied」退出"
    info "修复：sudo chown ${svc_user} ${CONFIG_FILE} && sudo chmod 600 ${CONFIG_FILE}"
  fi
  if $SUDO -u "$svc_user" test -w "$DATA_DIR" 2>/dev/null; then
    ok "数据目录可写"
  else
    bad "无法写入 $DATA_DIR —— 数据库无法创建/更新"
    info "修复：sudo chown -R ${svc_user} ${DATA_DIR}"
  fi
  if $SUDO -u "$svc_user" test -x "$BIN_DEST" 2>/dev/null; then
    ok "二进制可执行"
  else
    bad "二进制不可执行"
    info "修复：sudo chmod 755 ${BIN_DEST}"
  fi
else
  warn "系统中不存在用户 ${svc_user}，systemd 会直接启动失败"
fi

hr "4. 配置文件内容校验"
if [ -f "$CONFIG_FILE" ] && command -v python3 >/dev/null 2>&1; then
  if $SUDO python3 -c 'import json,sys;json.load(open(sys.argv[1]))' "$CONFIG_FILE" 2>/dev/null; then
    ok "JSON 格式合法"
    $SUDO python3 - "$CONFIG_FILE" <<'PY'
import json, sys
c = json.load(open(sys.argv[1]))
for k in ("uuid", "server_name", "web_port", "data_dir",
          "receive_enabled", "receive_port", "report_enabled"):
    print("  %-18s = %s" % (k, c.get(k)))
PY
    WEB_PORT="$($SUDO python3 -c 'import json,sys;print(json.load(open(sys.argv[1])).get("web_port",8080))' "$CONFIG_FILE")"
  else
    bad "JSON 格式非法 —— 服务会以「配置文件格式错误」退出"
  fi
else
  warn "配置文件不存在或缺少 python3，跳过校验"
fi

hr "5. 端口占用检查"
WEB_PORT="${WEB_PORT:-8080}"
if command -v ss >/dev/null 2>&1; then
  for port in "$WEB_PORT"; do
    if $SUDO ss -lntp 2>/dev/null | grep -q ":${port} "; then
      bad "端口 ${port} 已被占用："
      $SUDO ss -lntp 2>/dev/null | grep ":${port} " | sed 's/^/    /'
      info "若是残留的 server-monitor 进程，先执行：sudo systemctl stop ${APP_NAME}; sudo pkill -f ${BIN_DEST}"
    else
      ok "端口 ${port} 空闲"
    fi
  done
else
  warn "缺少 ss 命令，跳过后执行：sudo lsof -iTCP -sTCP:LISTEN -P -n"
fi

hr "6. 前台试运行（关键：程序会直接打印退出原因）"
if [ -x "$BIN_DEST" ]; then
  info "命令：$BIN_DEST --config $CONFIG_FILE --data-dir $DATA_DIR"
  info "---------- 程序输出开始 ----------"
  if command -v timeout >/dev/null 2>&1; then
    $SUDO timeout 3 "$BIN_DEST" --config "$CONFIG_FILE" --data-dir "$DATA_DIR" 2>&1 | sed 's/^/    /'
    rc=${PIPESTATUS[0]}
    info "---------- 程序输出结束（退出码 ${rc}，124 表示正常启动后被超时终止）----------"
    if [ "$rc" -eq 124 ]; then
      ok "程序可正常启动，问题可能出在 systemd 环境（单元配置 / 端口冲突 / 权限）"
    fi
  else
    warn "缺少 timeout 命令，请手动执行上面这条命令观察输出"
  fi
else
  bad "二进制不存在或不可执行，无法试运行"
fi

hr "7. 最近日志"
$SUDO journalctl -u "$APP_NAME" -n 30 --no-pager 2>&1 | sed 's/^/  | /'

hr "诊断结束"
printf '  常见结论：\n'
printf '    - 输出含「启动失败：读取配置文件失败 … permission denied」→ 单元里的 User= 与文件属主不一致（见第 3 节）\n'
printf '    - 输出含「启动失败：初始化数据库失败 …」→ 数据目录权限或文件系统问题（见第 2、3 节）\n'
printf '    - 输出含「Web 端口 %s 无法监听」→ 端口被占用或权限不足（见第 5 节）\n' "${WEB_PORT:-8080}"
printf '    - 想换 Web 端口：重新运行 install.sh 在交互中填写，或用 --web-port 启动一次\n'
printf '    - 输出含「启动失败：配置文件格式错误」→ 配置文件损坏（见第 4 节）\n'

# 服务器监控（Server Monitor）

运行在 Linux 服务器上的监控程序：采集本机 CPU / 内存状态、本地留存历史数据，并支持多台服务器互相上报、
集中在一个 Web 面板中展示。

- **开发语言**：Go
- **部署形态**：单二进制（无 CGO 依赖，可直接拷贝到目标服务器运行）
- **本地存储**：SQLite（[modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite) 纯 Go 实现）
- **外部依赖**：无（前端资源全部内嵌，不依赖任何 CDN）

---

## 1. 运行形态

程序不区分「监控模式」与「面板模式」，所有部署实例形态完全一致：

- 始终采集本机状态并写入本地 SQLite；
- 始终提供 Web 界面，登录后先进入**面板**；
- 面板数据 = 本机数据 + 其它服务器上报的数据；
- 若无人向本机上报，面板就只显示本机一张卡片；
- **首次启动自动生成 config 配置文件**，其中包含本机识别码（UUID v4），作为该服务器的唯一标识。

数据接收与数据上报各自独立，由设置页的开关分别控制，互不影响。
另有**对外查询 API**（`GET /api/servers`）可把本机存储的全部服务器数据交给第三方读取，
同样由设置页单独开关控制（见 7.2）。

---

## 2. 快速开始

### 2.1 本地直接运行（调试）

```bash
go build -o server-monitor .
./server-monitor init --config ./dev/config.json --data-dir ./dev/data \
                      --web-user smdadmin --web-pass 'your-password' --web-port 8080
./server-monitor --config ./dev/config.json --data-dir ./dev/data
```

浏览器打开 `http://127.0.0.1:8080`，用上面的账号密码登录。

> 开发机为 macOS 时，采集通过系统命令估算（`top` / `vm_stat`），仅用于流程验证；
> 生产环境（Linux）读取 `/proc/stat` 与 `/proc/meminfo`，为精确值。

### 2.2 编译发布产物

```bash
./build.sh          # 产物输出到 dist/
```

生成：

| 文件 | 说明 |
| --- | --- |
| `dist/server-monitor-linux-amd64` | Linux x86_64 |
| `dist/server-monitor-linux-arm64` | Linux ARM64 |
| `dist/server-monitor-linux-arm` | Linux ARMv7 |
| `dist/server-monitor-darwin-arm64` | macOS（仅调试用） |
| `dist/install.sh` | 安装 / 升级 / 管理脚本（与仓库根目录同一份） |
| `dist/SHA256SUMS.txt` | 上述二进制的 SHA-256，安装脚本下载后自动校验 |

### 2.3 部署到服务器（推荐：一行命令）

**不需要先下载任何文件**，在服务器终端（root 或 sudo 账户）直接执行：

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/zeisscai/server-monitor/main/install.sh)
```

脚本会自动识别 CPU 架构（amd64 / arm64 / armv7）、从 GitHub Release 下载对应版本、
校验 SHA-256，然后进入菜单：

```
  1) 全新安装
  2) 升级到最新版本（保留配置与数据）
  3) 管理（启停 / 日志 / 改端口 / 改密码 / 诊断 / 卸载）
  4) 查看运行状态
  0) 退出
```

选 `1` 后只需回答 **Web 端口** 与 **登录账号 / 密码**（回车全部取默认值，密码留空则随机生成），
其余（目录、systemd 单元、配置、自检、启动）全部自动完成，最后输出访问地址与账号密码。

**国内网络**：GitHub 直连较慢时加镜像前缀。

```bash
SMD_MIRROR=https://ghfast.top/ bash <(curl -fsSL https://raw.githubusercontent.com/zeisscai/server-monitor/main/install.sh)
```

**离线 / 内网**：把 `server-monitor-linux-amd64` 与 `install.sh` 上传到服务器，然后
`sudo bash install.sh install --bin ./server-monitor-linux-amd64`。

**批量部署 / 自动化**（不做任何交互）：

```bash
bash install.sh install --yes --port 8080 --user smdadmin --pass 'your-password'
```

| 参数 | 说明 |
| --- | --- |
| `--port N` | Web 端口，默认沿用已有配置，首次为 8080 |
| `--user U` / `--pass P` | 登录账号 / 密码；密码留空时沿用已有配置，没有则随机生成 |
| `--bin PATH` | 使用本地二进制，不联网下载 |
| `--version V` | 指定版本（如 `1.1.0`），默认取最新 Release |
| `--yes` | 全部使用默认值，不再逐项确认 |

端口输入带校验：非数字、超出 `1~65535`、与数据接收端口冲突都会被拒绝；
端口已被其它程序占用时会列出占用进程并让你确认
（若占用者是本程序的旧实例，则视为正常情况，安装时会自动停掉它）。

### 2.4 升级

```bash
bash install.sh upgrade
```

- 保留配置（账号、密码、端口、API-Key、本机 UUID）与全部历史数据；
- 升级前自动备份当前二进制（`.prev`）与配置（`config.json.bak.<时间戳>`）；
- 新版本若**启动自检失败或 HTTP 探活失败，会自动回滚到旧版本**并重新拉起服务；
- 已是最新版本时直接提示，不做任何改动。

### 2.5 日常管理

`bash install.sh manage` 进入管理菜单，覆盖绝大多数运维动作，不必再记 systemd 命令：

| 菜单项 | 等价操作 |
| --- | --- |
| 查看运行状态 | `status` 子命令：版本、服务状态、端口、Key（掩码）、数据库占用 |
| 启动 / 停止 / 重启 | `systemctl start/stop/restart server-monitor` |
| 查看实时日志 | `journalctl -u server-monitor -f` |
| 修改 Web 端口 | 写回配置并自动重启（不改变账号密码） |
| 重置登录密码 | 校验强度后写回配置并重启，旧登录态立即失效 |
| 查看完整配置 | 展示账号、密码、接收 / 查询 API-Key 等 |
| 系统诊断 | 版本、服务状态、端口监听、磁盘、最近 30 行日志 |
| 卸载 | 停服、删单元与二进制；**默认保留配置与数据**，删除需二次确认 |

常用 systemd 命令仍然有效（菜单只是把它们包了一层）：

```bash
systemctl status  server-monitor
systemctl restart server-monitor
journalctl -u server-monitor -n 100 -f
```

---

## 3. 目录与服务约定

程序二进制、配置文件、数据目录统一收敛在默认安装目录 `/opt/server-monitor/` 下：

| 项 | 路径 |
| --- | --- |
| 安装目录 | `/opt/server-monitor/` |
| 程序二进制 | `/opt/server-monitor/server-monitor` |
| 配置文件 | `/opt/server-monitor/config.json` |
| 数据目录 | `/opt/server-monitor/data/`（SQLite 存放） |
| systemd 单元 | `/etc/systemd/system/server-monitor.service` |

> 唯一的例外是 systemd 单元文件，它必须位于 `/etc/systemd/system/`，由 systemd 规定，无法并入安装目录。

systemd 单元：

```ini
[Unit]
Description=Server Monitor
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/opt/server-monitor/server-monitor --config /opt/server-monitor/config.json --data-dir /opt/server-monitor/data
Restart=always
RestartSec=5
# 千台上报时每台保持一条长连接，默认 1024 个句柄不够用。
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
```

常用管理命令：

```bash
systemctl status  server-monitor
systemctl restart server-monitor
journalctl -u server-monitor -n 100 -f
```

**幂等 / 升级**：`install.sh upgrade` 只替换二进制，配置文件与数据目录（含 SQLite）保持不变，不丢历史数据；
重复执行 `install.sh install` 也只会覆盖程序，配置沿用现有值（含密码）。
若检测到早期版本使用的分散目录（`/etc/server-monitor/config.json`、`/var/lib/server-monitor/`），
脚本会在新位置为空时自动继承，不会覆盖已有数据。

---

## 4. 配置文件

默认路径 `/opt/server-monitor/config.json`，权限 `600`（内含明文登录密码，便于部署后查询）。
`web_pass` 通过 `init` 写入时会强制校验强度（见 10.3）；手工编辑配置文件则不受校验，
但重启后旧登录态会因凭证指纹变化而立即失效。
文件不存在时程序会自动生成。

```json
{
  "uuid": "b6f2f65e-43ec-45c7-80e8-51cede6b2d23",
  "server_name": "web-01",
  "web_port": 8080,
  "web_user": "smdadmin",
  "web_pass": "your-password",
  "session_secret": "……",
  "data_dir": "/opt/server-monitor/data",

  "receive_enabled": false,
  "receive_port": 9100,
  "receive_api_key": "",

  "api_enabled": false,
  "api_key": "",

  "report_enabled": false,
  "report_target_ip": "",
  "report_target_port": 0,
  "report_interval": 5,
  "report_api_key": ""
}
```

| 配置 | 说明 |
| --- | --- |
| `uuid` | 本机识别码，首次启动自动生成，只读 |
| `server_name` | 本机在面板中的显示名称 |
| `web_port` | Web 界面服务端口 |
| `receive_enabled` | 数据接收开关，默认关闭 |
| `receive_port` | 数据接收端口（查询 API 共用此端口） |
| `receive_api_key` | 接收 API-Key，开启接收功能时自动生成（18 位），只读 |
| `api_enabled` | 对外查询 API（`GET /api/servers`）开关，默认关闭；与接收共用端口 |
| `api_key` | 查询 API-Key，开启查询 API 时自动生成（18 位），只读，**与接收 Key 不同** |
| `report_enabled` | 数据上报开关 |
| `report_target_ip` | 上报目标服务器 IP |
| `report_target_port` | 上报目标服务器接收端口 |
| `report_interval` | 上报频率，仅可选 2 / 5 / 10 秒，与采集频率绑定 |
| `report_api_key` | 目标服务器的接收 Key |

> 修改 `web_port` 可用 `--web-port` 参数或在部署脚本中重新设置（见第 5 节），
> 修改 `data_dir` 需编辑配置文件；两者均需重启服务生效。其余配置都可在设置页修改并即时生效。

---

## 5. 命令行

```
server-monitor [serve] [--config <配置文件>] [--data-dir <数据目录>] [--web-port <端口>]
    启动服务（默认命令）。配置文件不存在时自动生成（含本机 UUID）。
    --web-port 会覆盖并写回配置文件中的 Web 监听端口，省略时使用配置文件中的值。

server-monitor init --config <配置文件> --data-dir <数据目录>
                    --web-user <账号> --web-pass <密码> [--web-port <端口>]
    初始化配置与登录账号，供部署脚本调用。--web-port 省略时保留原配置端口。
    --web-pass 会做强度校验：至少 8 个字符、非常见弱口令、不得与账号相同、
    不得是同一字符的重复。省略 --web-pass 时沿用既有口令且跳过校验，
    以便历史遗留弱口令的部署也能顺利升级。

server-monitor version
    显示版本号。
```

默认：安装目录 `/opt/server-monitor/`，配置文件 `/opt/server-monitor/config.json`，数据目录 `/opt/server-monitor/data`。

端口取值一律要求在 `1~65535` 之间，非法值会直接报错退出，不会被静默改掉。

修改 Web 端口的两种方式：

```bash
# 方式一：管理菜单（推荐，自动写回配置并重启）
bash install.sh manage      # 选「6) 修改 Web 端口」

# 方式二：运行时覆盖，会写回配置文件
sudo systemctl stop server-monitor
sudo /opt/server-monitor/server-monitor --config /opt/server-monitor/config.json \
     --data-dir /opt/server-monitor/data --web-port 9000
sudo systemctl start server-monitor
```

> 端口写在配置文件里而不是 systemd 单元里，因此两种方式改完都只需重启服务即可长期生效。

---

## 6. 功能说明

### 6.1 数据采集

- 指标：**CPU 占用率**、**运行内存占用率**；
- 采集频率与上报频率绑定，统一使用上报设置中的频率（2 / 5 / 10 秒）；
- **上报功能关闭时，按 5 秒采集**；
- 本机数据始终记录，不依赖接收 / 上报开关状态。

另外还会采集三项**低频变化**的系统信息，用于在面板上区分机器：

| 信息 | 来源（Linux） | 变化时机 |
| --- | --- | --- |
| 发行版本 | `/etc/os-release` 的 `PRETTY_NAME` | 系统升级 |
| 内核版本 | `/proc/sys/kernel/osrelease` | 升级内核并重启 |
| 运行时长 | `/proc/stat` 的 `btime`（缺失时用 `/proc/uptime` 反推） | 重启 |

这三项**不会随每次上报重复发送**（见 6.4）：采集器内部缓存 10 分钟刷新一次，
上报端只在「内容变化」或「距上次发送超过 30 分钟」时才随请求带一次。
运行时长也不是每次测量出来的，而是由**开机时刻**实时推算，
因此面板上显示的数字永远是最新的，却几乎不产生额外流量。

### 6.2 本地历史数据

- SQLite 存储，区分数据来源（本机 / 上报）；
- **所有历史数据永久保留**，不做清理；
- 启用 WAL 模式；写入由单线程攒批提交（见第 12 节），千台上报也不会丢点；
- 每台服务器的「最新状态」另外维护在 `server_latest` 表，面板查询与历史数据量无关。

### 6.3 数据接收（默认关闭）

- 开启后监听指定端口，设置页显示「已开启 · 监听 &lt;端口&gt;」与**数据库占用空间**；
- 开启时自动生成 18 位 API-Key，设置页展示并提供**复制按钮**；
- 关闭时不接收任何外部数据、API-Key 清空（下次开启重新生成），设置页不显示数据库占用；
- 该端口与「对外查询 API」共用：两者任一开启即监听，都关闭才完全不开放端口（见 7.2）。

### 6.4 数据上报

- HTTP POST，目标 `http://<IP>:<端口>/api/report`；
- 请求头携带 `X-API-Key`，请求体：

```json
{
  "uuid": "b6f2f65e-43ec-45c7-80e8-51cede6b2d23",
  "name": "web-01",
  "cpu": 12.3,
  "mem": 45.6,
  "ts": 1790090052294,
  "sys": {
    "os": "Ubuntu 22.04.3 LTS",
    "kernel": "5.15.0-91-generic",
    "boot_at": 1790000000000,
    "uptime": 90052
  }
}
```

> 同时兼容 `server_uuid` / `server_name` / `cpu_usage` / `mem_usage` / `created_at` 等下划线字段名。

`sys` 是**可选**字段：只有发行版 / 内核 / 开机时刻发生变化、或距上次发送超过 30 分钟时
才会带上。接收端缺少该字段时**保留此前记录的值**，不会被清空，
因此第三方接入方即使永远不发 `sys`，也只是在面板上显示「系统信息未知」。

| 字段 | 说明 |
| --- | --- |
| `sys.os` | 发行版本，最长 64 字符 |
| `sys.kernel` | 内核版本，最长 64 字符 |
| `sys.boot_at` | 开机时刻（Unix 毫秒）；接收端会丢弃未来时间点，避免运行时长算成负数 |
| `sys.uptime` | 运行时长（秒），仅供参考；接收端以 `boot_at` 推算为准 |

### 6.5 面板

- **主界面**：卡片列表展示所有服务器的名称、CPU 占用、内存占用、**发行版本 / 内核 / 运行时长**、数据更新时间，每 5 秒自动刷新；
- **详情页**：点击卡片进入，查看该服务器历史记录；支持最近 1 小时 / 24 小时 / 7 天 / 1 个月
  以及自定义时间范围查询，**默认最近一个月**；展示趋势曲线、统计值与最近 200 条明细。

### 6.6 Web 登录

账号密码由部署初始化脚本写入配置；登录态使用 HMAC 签名 Cookie（有效期 7 天），不依赖服务端会话存储。
Cookie 载荷中绑定了当前凭证的指纹，因此**修改口令（重新执行 `init`，或编辑配置后重启）
会让此前签发的所有登录态立即失效**，不必等 7 天自然过期。退出登录为带 CSRF 校验的 POST。

---

## 7. 数据接口

接收与查询共用同一个端口（默认 9100），但**开关与 API-Key 各自独立**。

### 7.1 接收上报

| 接口 | 方法 | 说明 |
| --- | --- | --- |
| `/api/report` | POST | 仅在数据接收开关开启、监听端口开放时可用；`X-API-Key` 校验；以 UUID 识别服务器 |

校验规则：

| 情况 | 响应 |
| --- | --- |
| 非 POST 方法 | `405 {"ok":false,"error":"仅支持 POST"}` |
| 接收功能未开启 / 无 Key | `403` |
| API-Key 不匹配 | `401 {"ok":false,"error":"API-Key 校验失败"}` |
| UUID 非标准格式 | `400 {"ok":false,"error":"服务器识别码不合法"}` |
| cpu / mem 不在 0~100 | `400` |
| 上报端时间偏差超过 1 小时 | 以接收端时间为准（不拒绝） |
| `sys` 缺失 | 保留该服务器已有的系统信息（不拒绝、不清空） |
| 成功 | `200 {"ok":true}` |

### 7.2 查询全部服务器（默认关闭）

设置页「数据接收」卡片内的**对外查询 API** 开关控制，用于让第三方（自建的看板、
巡检脚本、上报到上级平台等）一次性取走本机存储的**全部**服务器数据，含本机自身。

```bash
curl -H 'X-API-Key: <查询 API-Key>' http://<面板服务器IP>:9100/api/servers
```

```json
{
  "ok": true,
  "now": 1793848646500,
  "self": "dca0cca4-b574-45f4-947d-be59d8cc40ea",
  "self_name": "panel-01",
  "count": 2,
  "servers": [
    {
      "uuid": "dca0cca4-b574-45f4-947d-be59d8cc40ea",
      "name": "panel-01",
      "cpu": 29.4,
      "mem": 71.5,
      "source": "local",
      "ts": 1793848645803,
      "count": 9,
      "is_self": true,
      "os": "Ubuntu 22.04.3 LTS",
      "kernel": "5.15.0-91-generic",
      "boot_at": 1793085629000,
      "uptime": 763017
    }
  ]
}
```

| 字段 | 说明 |
| --- | --- |
| `now` | 服务端当前时间（Unix 毫秒），便于调用方自行计算数据新鲜度 |
| `self` / `self_name` | 本机识别码与名称 |
| `count` | 返回的服务器数量 |
| `servers[].cpu` / `mem` | 最新 CPU / 内存占用率（%） |
| `servers[].ts` | 该服务器最近一条数据的时间（Unix 毫秒） |
| `servers[].count` | 该服务器累计记录条数 |
| `servers[].os` / `kernel` | 发行版本 / 内核版本，未知时为空字符串 |
| `servers[].boot_at` | 开机时刻（Unix 毫秒），未知时为 0 |
| `servers[].uptime` | 运行时长（秒），**按请求时刻实时推算**；`boot_at` 未知时为 0 |

| 情况 | 响应 |
| --- | --- |
| 非 GET / HEAD | `405` |
| 查询 API 未开启 | `403 {"ok":false,"error":"查询 API 未开启"}` |
| Key 不匹配（含误用接收 Key） | `401` |
| 成功 | `200`，响应头带 `Cache-Control: no-store` |

**为什么不用接收 Key？** 接收 Key 必须分发给每一台上报服务器，它只应具备「写」权限。
若查询接口复用同一个 Key，任何一台被攻陷（或运维交接后仍留有权限）的服务器
都能拉走整个机群的状况；而且关闭再开启接收功能时接收 Key 会被重新生成，
会把第三方查询方一起打断。因此这里生成**独立的 18 位 Key**，关掉查询开关即作废。

Key 只接受 `X-API-Key` 请求头或 `Authorization: Bearer`，**不支持 `?key=` 查询参数**：
URL 会进入各级访问日志与浏览器历史，把长期有效的凭据放在里面等于公开。

> 需要把这份接口交给第三方（外部看板、巡检脚本、上级平台）时，
> 直接提供 **[API.md](API.md)** 即可：其中包含访问规范、字段表、状态码、
> 运行时长/在线状态的推算方法与参考解析代码。

---

## 8. 数据结构

表 `records`：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `id` | INTEGER | 自增主键 |
| `server_uuid` | TEXT | 服务器唯一识别码 |
| `server_name` | TEXT | 服务器名称 |
| `cpu_usage` | REAL | CPU 占用率（%） |
| `mem_usage` | REAL | 内存占用率（%） |
| `source` | TEXT | 数据来源：`local`（本机）/ `remote`（上报） |
| `created_at` | INTEGER | 采集时间（Unix 毫秒） |

索引：`(server_uuid, created_at)`、`(created_at)`。

表 `server_latest`（每台服务器一行，由写入线程在同一事务内维护）：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `server_uuid` | TEXT | 主键 |
| `server_name` | TEXT | 最近一次上报的名称 |
| `cpu_usage` | REAL | 最新 CPU 占用率 |
| `mem_usage` | REAL | 最新内存占用率 |
| `source` | TEXT | 最近一条记录的来源 |
| `created_at` | INTEGER | 最近一条记录的时间（乱序上报不会回退） |
| `cnt` | INTEGER | 该服务器累计记录数 |

> 老版本数据库首次被新版本打开时，会自动从 `records` 回填这张表（每台一行，只做一次）。
> 面板与 `/api/overview` 只读这张表，因此即使历史数据上亿行，刷新依旧是毫秒级。

表 `server_info`（每台服务器一行，系统信息）：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `server_uuid` | TEXT | 主键 |
| `os_name` | TEXT | 发行版本，如 `Ubuntu 22.04.3 LTS` |
| `kernel` | TEXT | 内核版本，如 `5.15.0-91-generic` |
| `boot_at` | INTEGER | 开机时刻（Unix 毫秒），0 表示未知 |
| `updated_at` | INTEGER | 该信息的采集 / 接收时间 |

> 这三项只在系统升级或重启后变化，因此**不随每条采样记录存一份**，
> 而是单独一张表按 uuid 维护；上报体里没带 `sys` 时只更新 `updated_at` 之外的字段保持原值。
> 表结构随程序自动创建，老库升级无需手工迁移。

数据库文件：`<数据目录>/monitor.db`（含 `-wal` / `-shm` 附属文件）。

---

## 9. 目录结构

```
.
├── main.go                      CLI 入口（serve / init / version）
├── rlimit_unix.go               启动时抬高文件句柄上限（Linux / macOS）
├── build.sh                     交叉编译脚本
├── install.sh                   安装 / 升级 / 管理脚本（单入口，可 curl 直接运行）
├── deploy/diagnose.sh           故障诊断脚本
├── REQUIREMENTS.md              开发需求文档
├── API.md                       第三方接入说明（查询 API 的访问与解析规范）
└── internal
    ├── config/                  配置文件生成、读取、校验、持久化
    ├── collect/                 指标采集（Linux /proc，macOS 兜底）
    │   ├── sysinfo*.go          发行版 / 内核 / 开机时刻（含纯解析逻辑与单测）
    ├── store/                   SQLite 存储层（单写线程 + 攒批提交 + server_latest + server_info）
    ├── engine/                  采集 / 上报 / 接收 / 查询 API 运行时
    │   └── load_test.go         千台规模压测（SMD_LOAD=1 开启）
    ├── security/                常量时间比较、安全审计日志、客户端地址解析
    └── web/                     Web 服务
        ├── web.go               路由、认证、页面与接口
        ├── ratelimit.go         登录失败限流（IP 指数退避 + 全局滑动窗口）
        ├── templates/           页面模板（内嵌）
        └── static/              CSS / JS（内嵌）
```

---

## 10. 安全与非功能说明

- 接收端口默认关闭，避免不必要的对外暴露；
- 上报、接收与查询通道均使用 API-Key 认证；API-Key 与登录口令均以**常量时间**比较，
  避免通过响应时间逐字节推断正确值；
- **上报用的接收 Key 与外部查询用的 API-Key 互相独立**，写权限不会顺带获得读全量数据的权限（见 7.2）；
- 服务器以 UUID 唯一识别，名称仅作展示；
- `config.json` 权限 600，仅所有者可读；
- 登录接口具备暴力破解防护（见 10.1）；
- 程序不做常规运行日志，但安全事件会写入审计日志（见 10.2）。

### 10.1 登录防护

登录失败按来源 IP 指数退避，并叠加全局滑动窗口限流，**刻意不做硬锁定**：

| 机制 | 取值 | 说明 |
| --- | --- | --- |
| 免罚次数 | 3 次 | 前 3 次失败只提示错误，容忍手误 |
| IP 退避 | 2 秒起逐次翻倍，上限 15 分钟 | 连续失败后被拒绝，返回 `429` 与 `Retry-After` |
| 全局窗口 | 60 秒内累计 20 次失败 | 分布式爆破（不断更换 IP）时暂时拒绝所有登录尝试 |
| 状态回收 | 空闲 1 小时 | 清理长期不活跃的 IP 记录，避免内存增长 |

被限流拦下的请求**不计入失败数**，因此惩罚会随时间自然衰减：合法管理员不会被永久
锁在门外，全局窗口最长也只持续一个窗口。退避状态保存在内存中，重启服务即清空。

限流按来源 IP 判定，**不采信 `X-Forwarded-For`**：该头部可被客户端随意伪造，
用它做限流等于把绕过限流的能力交给攻击者。因此若把本服务置于反向代理之后，
所有请求会共享代理 IP 的限流额度——此时应调整 10.1 中的阈值，或由代理层承担限流。

### 10.2 安全审计日志

程序不做常规运行日志，但下列安全事件会以 `[安全]` 前缀写入 **stderr**
（systemd 下由 journald 收集）：

| 事件 | 说明 |
| --- | --- |
| `login_success` | 登录成功 |
| `login_failure` | 登录失败，含累计失败次数 |
| `login_backoff` | 触发指数退避 |
| `login_throttled` | 请求被限流拦下（按全局 30 秒一次输出） |
| `report_auth_failure` | 接收端 API-Key 校验失败（按 IP 每分钟一次输出） |
| `api_auth_failure` | 查询 API-Key 校验失败（按 IP 每分钟一次输出） |

```bash
sudo journalctl -u server-monitor -n 100 -f | grep '\[安全\]'
```

日志中的字段值会剔除控制字符并截断，避免攻击者通过用户名等可控输入注入换行符
伪造日志行；**密码与 API-Key 明文永不记入日志**。可被未认证请求任意触发的事件
按频率上限输出，防止攻击者用海量请求把日志写成洪水、反过来打满磁盘。

### 10.3 其他 Web 加固

| 项 | 做法 |
| --- | --- |
| 口令强度 | `init` 强制校验：至少 8 个字符、非常见弱口令、不得与账号相同、不得是同一字符重复；部署脚本在录入阶段即提示重填 |
| CSRF | 设置保存与退出登录均为 POST，且要求表单携带由会话令牌派生的 CSRF token；校验失败返回 `403` 且不产生任何副作用 |
| 退出登录 | 改为带 CSRF 的 POST；`GET /logout` 只做无害跳转，避免跨站顶层跳转强制登出 |
| 会话失效 | Cookie 载荷绑定「账号 + 口令 + 会话密钥」指纹，任一变更都让已签发登录态立即失效 |
| 安全响应头 | `Content-Security-Policy`、`Permissions-Policy`、`X-Content-Type-Options`、`X-Frame-Options`、`Referrer-Policy` |
| HSTS | 仅在请求确实走 TLS 时下发（明文请求上该头部会被浏览器忽略，无条件下发反而掩盖尚未启用 HTTPS 的事实） |

CSP 中 `script-src` 收紧为 `'self'`：模板与 `app.js` 里没有任何内联脚本或内联事件处理器。
`style-src` 保留了 `'unsafe-inline'`，因为进度条宽度与图表提示配色使用了内联 `style`
属性（见 `panel.html`、`app.js`），去掉会直接破坏渲染。样式注入的危害远小于脚本注入，
这一取舍是有意的：真正有价值的是锁死脚本来源。

> 若在设置页看到「表单校验失败（页面可能已过期）」，说明提交时缺少或携带了错误的
> CSRF token（常见于页面停留过久、或换个浏览器标签提交）。刷新页面重新提交即可。

---

## 11. 故障排查

### 11.1 服务反复重启

`systemctl status` 出现下面这种状态，说明程序每次启动都以非 0 退出，被 systemd 按 `Restart=always` 反复拉起：

```
Active: activating (auto-restart) (Result: exit-code) since ...
Process: 8087 ExecStart=/opt/server-monitor/server-monitor ... (code=exited, status=1/FAILURE)
```

**程序启动阶段的每个失败分支都会把原因写到 stderr，systemd 会把它收进 journal，所以第一条命令永远是看日志：**

```bash
sudo journalctl -u server-monitor -n 50 --no-pager
```

也可以直接在前台跑一次，错误会立刻打在屏幕上：

```bash
sudo systemctl stop server-monitor
sudo /opt/server-monitor/server-monitor --config /opt/server-monitor/config.json --data-dir /opt/server-monitor/data
```

另外提供了诊断脚本，一次输出单元配置、文件权限、服务身份可访问性、配置合法性、
端口占用、前台试运行输出与最近日志：

```bash
sudo bash deploy/diagnose.sh
```

### 11.2 常见报错对照

| journal 中的输出 | 原因 | 处理 |
| --- | --- | --- |
| `启动失败：读取配置文件失败 …: permission denied` | 单元里设置了 `User=`，但 `config.json` 属主是 root 且权限 600 | `sudo chown <服务用户> /opt/server-monitor/config.json` |
| `启动失败：初始化数据库失败 …/monitor.db: …` | 数据目录不可写，或数据库文件损坏 | `sudo chown -R <服务用户> /opt/server-monitor/data`；损坏时移走 `monitor.db*` 后重启 |
| `服务异常退出：Web 端口 8080 无法监听（可能已被其它程序占用）：listen tcp :8080: bind: address already in use` | Web 端口被占用（常见于残留的旧进程或其他程序占用 8080） | `sudo ss -lntp \| grep 8080` 看是谁占用；换端口可在部署脚本中重填，或用 `--web-port` 启动一次（见第 5 节） |
| `启动失败：配置文件格式错误 …` | 配置文件被手工改坏 | 修正 JSON，或删除后重启让程序重新生成（会换新 UUID） |
| 日志里只有启动横幅、没有报错 | 程序本身正常，问题在 systemd 单元 | 检查 `ExecStart` 参数与单元里是否有 `User=` |

> 判断技巧：日志里若出现「服务器监控 vX.Y.Z 已启动」横幅，说明配置与数据库都已就绪，
> 失败点在**监听端口**；若横幅之前就出现「启动失败：」，则失败在配置 / 数据库 / 模板阶段。

### 11.3 部署脚本已内置自检

`install.sh` 在启动 systemd 服务前会用服务身份在前台试跑 3 秒（`smoke_test`），
确认配置可读、数据库可写、Web 端口可监听；任一步失败会直接打印程序输出并终止部署，
不会再出现"部署显示成功、服务却反复重启"的情况。

### 11.4 登录提示「尝试过于频繁」

这是登录防护（见 10.1）生效，不是程序故障：

| 现象 | 原因 | 处理 |
| --- | --- | --- |
| 登录返回 `429`，提示「登录尝试过于频繁，请 N 秒后重试」 | 该来源 IP 连续失败次数过多，处于指数退避中 | 等待提示的秒数后重试；忘记密码可查 `config.json` 中的 `web_pass` |
| 所有人登录都返回 `429` | 60 秒内全局失败次数达到上限，可能正被分布式爆破 | 最多等 1 分钟自动恢复；用 `journalctl -u server-monitor \| grep '\[安全\]'` 确认来源 |

应急时可 `systemctl restart server-monitor` 立即清空退避状态（退避记录只在内存中）。

---

## 12. 千台规模：并发模型与容量

### 12.1 写入模型

接收端把「HTTP 处理」与「落盘」解耦，所有写入收敛到一个写线程：

```
HTTP 上报 ──校验──▶ 写入队列(16384) ──▶ 单写线程
                                          ├─ 攒批(≤512 条 / 3ms) ─▶ 一次 BEGIN/COMMIT
                                          ├─ server_latest（每台一行，同事务维护）
                                          └─ server_info（系统信息，仅在变化时写入）
只读查询 ─────────────────────────────▶ 连接池（WAL，一写多读）
```

- **单写线程**：SQLite 只允许一个写者，与其让几百个 goroutine 抢一把锁，不如让它们排队进
  一个 channel，由唯一的写线程顺序提交；
- **攒批提交**：把「每条一次 commit」变成「每批一次 commit」。空载时批很小、延迟低；
  高负载时批自然变大，吞吐随并发上升；
- **物化最新值**：面板不再对历史表做 `JOIN + 相关子查询`（旧实现要扫每台服务器的全部历史），
  改成读一张 1000 行的表；
- **读写分离**：读走连接池里的其它连接，WAL 下与写入互不阻塞，面板刷新不再卡住上报；
- **背压而非丢弃**：队列满时 `Insert` 阻塞，HTTP 请求随之变慢，但不会丢数据。
  只有事务提交成功后才会返回 `200`，即**已确认的上报一定已经落盘**。

对外查询接口（`/api/servers`）的开销同样与历史数据量无关：只读 `server_latest`
（1000 行）与 `server_info`（每台一行）两张小表，在内存里按 uuid 关联，
不做 JOIN、不碰 `records`。因此第三方按秒轮询也不会拖慢上报写入。

### 12.2 实测数据

环境：macOS（Apple Silicon），1000 台模拟上报（每 2 秒一次，约 440 req/s），
数据库预置 200 万条历史，面板每 500ms 刷新一次：

| 指标 | 优化前 | 优化后 |
| --- | --- | --- |
| 上报延迟 p50 | 50 ms | **3.8 ms** |
| 上报延迟 p95 | 339 ms | **4.5 ms** |
| 上报延迟 p99 | 497 ms | **5.9 ms** |
| 上报延迟 max | 558 ms | **8.6 ms** |
| 面板 `Latest()` 最慢一次 | 596 ms | **7 ms** |
| 1000 台同时重连（突发） | 成功 1000 / 失败 0 | 成功 1000 / 失败 0 |
| 吞吐上限（1000 并发持续压） | 11.7k req/s（含失败） | **25.4k req/s** |
| `Latest()` @1000 台 / 200 万条 | 490 ms | **1.0 ms** |
| 落库完整性 | 5000 / 5000 | 2005000 / 2005000 |

> 空载单条上报的延迟会从 0.27ms 变成约 3.9ms —— 这是攒批窗口（3ms）的代价，
> 换来的是高并发下不再排队、以及面板查询不再拖慢写入。对 2 秒一次的上报频率可以忽略。

复现方式（需显式开启，默认跳过）：

```bash
SMD_LOAD=1 go test ./internal/engine -run TestLoadReports -v -timeout 15m
SMD_LOAD=1 SMD_SEED=2000000 SMD_PANEL=1 go test ./internal/engine -run TestLoadReports -v
SMD_LOAD=1 SMD_WORKERS=1000 SMD_SECS=6 go test ./internal/engine -run TestLoadThroughput -v
SMD_LOAD=1 go test ./internal/engine -run TestLoadLatest -v
```

### 12.3 部署侧需要配合的参数

| 项 | 建议值 | 说明 |
| --- | --- | --- |
| `LimitNOFILE`（systemd） | `65535` | 程序启动时会自动把软限制抬到硬限制；单元里仍需放开硬限制，否则抬不上去 |
| `net.core.somaxconn` | `4096` | 千台同时重连（如接收端重启）时的 accept 队列上限，默认 1024 可能丢连接 |
| 上报频率 | 5 或 10 秒 | 千台规模下 2 秒一次意味着 500 req/s，5 秒只需 200 req/s，更从容 |

`deploy/diagnose.sh` 第 8 节会直接打印当前 `LimitNOFILE`、`somaxconn` 与接收端口的长连接数。

### 12.4 容量参考

按实测 25k req/s 的接收上限，单实例可承受：

| 上报频率 | 理论上限台数 | 建议运行区间 |
| --- | --- | --- |
| 2 秒 | 约 50000 台 | ≤ 5000 台 |
| 5 秒 | 约 125000 台 | ≤ 10000 台 |
| 10 秒 | 约 250000 台 | ≤ 20000 台 |

真正的长期瓶颈不是写入，而是**历史数据体积**：1000 台 × 2 秒 ≈ 每天 4300 万行、约 2~3 GB。
按需求「历史数据永久保留」，请预留磁盘并定期备份 `monitor.db`。


---

## 13. 发布与版本

版本号只有一个来源：`git tag`（`v1.1.0` 的 `1.1.0`）。`build.sh` 会把它注入二进制的
`main.version`，因此 `server-monitor version` 打印的始终是真实版本。

```bash
# 1. 构建（产物在 dist/，同时生成 SHA256SUMS.txt）
VERSION=1.1.0 ./build.sh

# 2. 打 tag 并推送
git tag -a v1.1.0 -m 'v1.1.0'
git push origin v1.1.0

# 3. 在 GitHub 上基于该 tag 创建 Release，上传 dist/ 下这些文件：
#    server-monitor-linux-amd64 / -linux-arm64 / -linux-arm
#    SHA256SUMS.txt
```

> `install.sh` 依赖 Release 的**资产命名**（`server-monitor-linux-<arch>`）与 `SHA256SUMS.txt`。
> 若改了命名规则，必须同步修改 `install.sh` 里的 `acquire_binary`。
> 安装脚本通过 `https://github.com/<repo>/releases/latest` 的重定向解析最新版本，
> 不消耗 GitHub API 配额。

### 13.1 更新日志

| 版本 | 内容 |
| --- | --- |
| v1.1.0 | 新增服务器发行版本 / 内核版本 / 运行时长（低频上报）；新增对外查询 API `GET /api/servers`（独立开关与 Key）；面板与详情页展示系统信息；安装脚本重写为单入口菜单式（自动下载、升级回滚、离线安装） |
| v1.0.0 | 首个版本：采集 / 上报 / 接收、面板与详情页、千台并发写入模型 |

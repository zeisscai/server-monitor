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
| `dist/install.sh` | 部署初始化脚本 |

### 2.3 部署到服务器

把 `server-monitor-linux-amd64` 与 `install.sh` 上传到目标服务器，然后在交互式终端执行：

```bash
sudo bash install.sh
```

脚本会依次询问 **Web 登录账号**（默认 `smdadmin`）、**Web 登录密码**（直接回车则随机生成）、
**待部署的二进制文件路径**、**Web 监听端口**（默认沿用已有配置，首次部署为 `8080`），
随后自动完成目录创建、配置写入、systemd 服务注册与启动。

端口输入带校验：非数字、超出 `1~65535`、与数据接收端口冲突都会被拒绝并要求重填；
若端口已被其它程序占用，会列出占用进程并让你确认或换一个端口
（若占用者是本程序的旧实例，则视为正常情况，部署时会自动停掉它）。

部署完成后输出访问地址、账号密码、各目录位置与常用管理命令。

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

[Install]
WantedBy=multi-user.target
```

常用管理命令：

```bash
systemctl status  server-monitor
systemctl restart server-monitor
journalctl -u server-monitor -n 100 -f
```

**幂等 / 升级**：重复执行 `install.sh` 会覆盖二进制，但配置文件与数据目录（含 SQLite）保留，不丢历史数据。
若检测到早期版本使用的分散目录（`/etc/server-monitor/config.json`、`/var/lib/server-monitor/`），
脚本会在新位置为空时自动继承，不会覆盖已有数据。

---

## 4. 配置文件

默认路径 `/opt/server-monitor/config.json`，权限 `600`（内含明文登录密码，便于部署后查询）。
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
| `receive_port` | 数据接收端口 |
| `receive_api_key` | 接收 API-Key，开启接收功能时自动生成（18 位），只读 |
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

server-monitor version
    显示版本号。
```

默认：安装目录 `/opt/server-monitor/`，配置文件 `/opt/server-monitor/config.json`，数据目录 `/opt/server-monitor/data`。

端口取值一律要求在 `1~65535` 之间，非法值会直接报错退出，不会被静默改掉。

修改 Web 端口的两种方式：

```bash
# 方式一：运行时覆盖，会写回配置文件（推荐）
sudo systemctl stop server-monitor
sudo /opt/server-monitor/server-monitor --config /opt/server-monitor/config.json \
     --data-dir /opt/server-monitor/data --web-port 9000
sudo systemctl start server-monitor

# 方式二：直接用部署脚本重新执行一遍，在交互中填写新端口
sudo bash install.sh
```

> 端口写在配置文件里而不是 systemd 单元里，因此两种方式改完都只需重启服务即可长期生效。

---

## 6. 功能说明

### 6.1 数据采集

- 指标：**CPU 占用率**、**运行内存占用率**；
- 采集频率与上报频率绑定，统一使用上报设置中的频率（2 / 5 / 10 秒）；
- **上报功能关闭时，按 5 秒采集**；
- 本机数据始终记录，不依赖接收 / 上报开关状态。

### 6.2 本地历史数据

- SQLite 存储，区分数据来源（本机 / 上报）；
- **所有历史数据永久保留**，不做清理；
- 启用 WAL 模式，串行化写入，保证采集不丢点。

### 6.3 数据接收（默认关闭）

- 开启后监听指定端口，设置页显示「已开启 · 监听 &lt;端口&gt;」与**数据库占用空间**；
- 开启时自动生成 18 位 API-Key，设置页展示并提供**复制按钮**；
- 关闭时不接收任何外部数据、不开放端口、API-Key 清空（下次开启重新生成），设置页不显示数据库占用。

### 6.4 数据上报

- HTTP POST，目标 `http://<IP>:<端口>/api/report`；
- 请求头携带 `X-API-Key`，请求体：

```json
{
  "uuid": "b6f2f65e-43ec-45c7-80e8-51cede6b2d23",
  "name": "web-01",
  "cpu": 12.3,
  "mem": 45.6,
  "ts": 1790090052294
}
```

> 同时兼容 `server_uuid` / `server_name` / `cpu_usage` / `mem_usage` / `created_at` 等下划线字段名。

### 6.5 面板

- **主界面**：卡片列表展示所有服务器的名称、CPU 占用、内存占用、数据更新时间，每 5 秒自动刷新；
- **详情页**：点击卡片进入，查看该服务器历史记录；支持最近 1 小时 / 24 小时 / 7 天 / 1 个月
  以及自定义时间范围查询，**默认最近一个月**；展示趋势曲线、统计值与最近 200 条明细。

### 6.6 Web 登录

账号密码由部署初始化脚本写入配置；登录态使用 HMAC 签名 Cookie（有效期 7 天），不依赖服务端会话存储。

---

## 7. 接收接口

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
| 成功 | `200 {"ok":true}` |

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

数据库文件：`<数据目录>/monitor.db`（含 `-wal` / `-shm` 附属文件）。

---

## 9. 目录结构

```
.
├── main.go                      CLI 入口（serve / init / version）
├── build.sh                     交叉编译脚本
├── deploy/install.sh            部署初始化脚本（交互式 + systemd）
├── REQUIREMENTS.md              开发需求文档
└── internal
    ├── config/                  配置文件生成、读取、校验、持久化
    ├── collect/                 指标采集（Linux /proc，macOS 兜底）
    ├── store/                   SQLite 存储层
    ├── engine/                  采集 / 上报 / 接收运行时
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
- 上报与接收通道均使用 API-Key 认证；API-Key 与登录口令均以**常量时间**比较，
  避免通过响应时间逐字节推断正确值；
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

```bash
sudo journalctl -u server-monitor -n 100 -f | grep '\[安全\]'
```

日志中的字段值会剔除控制字符并截断，避免攻击者通过用户名等可控输入注入换行符
伪造日志行；**密码与 API-Key 明文永不记入日志**。可被未认证请求任意触发的事件
按频率上限输出，防止攻击者用海量请求把日志写成洪水、反过来打满磁盘。

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


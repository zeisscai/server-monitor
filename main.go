// 服务器监控程序：采集本机 CPU / 内存状态，本地留存历史数据，
// 并支持与其它服务器互相上报 / 接收数据，通过 Web 界面集中展示。
//
// 单二进制部署，运行在 Linux 服务器上。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"server-monitor/internal/config"
	"server-monitor/internal/engine"
	"server-monitor/internal/store"
	"server-monitor/internal/web"
)

const version = "1.0.0"

const usageText = `服务器监控 (server-monitor) v` + version + `

用法：
  server-monitor [serve] [--config <配置文件>] [--data-dir <数据目录>] [--web-port <端口>]
      启动服务（默认命令）。配置文件不存在时自动生成（含本机 UUID）。
      --web-port 会覆盖并写回配置文件中的 Web 监听端口，省略时使用配置文件中的值。

  server-monitor init --config <配置文件> --data-dir <数据目录>
                      --web-user <账号> --web-pass <密码> [--web-port <端口>]
      初始化配置与登录账号，供部署脚本调用。--web-port 省略时保留原配置端口。

  server-monitor version
      显示版本号。

默认路径：
  配置文件  ` + config.DefaultConfigPath + `
  数据目录  ` + config.DefaultDataDir + `
`

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}

	switch cmd {
	case "serve":
		os.Exit(runServe(args))
	case "init":
		os.Exit(runInit(args))
	case "version", "-v", "--version":
		fmt.Println("server-monitor v" + version)
	case "help", "-h", "--help":
		fmt.Print(usageText)
	default:
		fmt.Fprintf(os.Stderr, "未知命令：%s\n\n%s", cmd, usageText)
		os.Exit(2)
	}
}

func runServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", config.DefaultConfigPath, "配置文件路径")
	dataDir := fs.String("data-dir", "", "数据目录（SQLite 存放位置）")
	webPort := fs.Int("web-port", 0, "Web 监听端口（0 表示使用配置文件中的端口）")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *webPort != 0 && !validPort(*webPort) {
		fmt.Fprintf(os.Stderr, "启动失败：--web-port 必须在 1~65535 之间，当前为 %d\n", *webPort)
		return 1
	}

	// 首次运行自动生成配置文件（含本机识别码 UUID）。
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "启动失败：", err)
		return 1
	}
	c := cfg.Snapshot()

	// --web-port 覆盖配置文件中的端口，并写回配置，保证手动运行与 systemd 启动行为一致。
	if *webPort != 0 && *webPort != c.WebPort {
		if _, err := cfg.Update(func(x *config.Config) { x.WebPort = *webPort }); err != nil {
			fmt.Fprintln(os.Stderr, "保存 Web 端口失败：", err)
			return 1
		}
		fmt.Printf("已更新 Web 监听端口：%d -> %d\n", c.WebPort, *webPort)
		c = cfg.Snapshot()
	}

	dd := c.DataDir
	if strings.TrimSpace(*dataDir) != "" && *dataDir != c.DataDir {
		dd = *dataDir
		if _, err := cfg.Update(func(x *config.Config) { x.DataDir = *dataDir }); err != nil {
			fmt.Fprintln(os.Stderr, "保存数据目录失败：", err)
			return 1
		}
	}

	st, err := store.Open(dd)
	if err != nil {
		fmt.Fprintln(os.Stderr, "启动失败：", err)
		return 1
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	eng := engine.New(ctx, cfg, st)
	eng.Apply()

	ws, err := web.New(cfg, st, eng)
	if err != nil {
		fmt.Fprintln(os.Stderr, "启动失败：", err)
		return 1
	}

	cur := cfg.Snapshot()
	fmt.Printf("服务器监控 v%s 已启动\n", version)
	fmt.Printf("  配置文件 : %s\n", cfg.Path())
	fmt.Printf("  数据目录 : %s\n", dd)
	fmt.Printf("  访问地址 : http://<本机IP>:%d\n", cur.WebPort)
	fmt.Printf("  识别码   : %s\n", cur.UUID)

	if err := ws.Run(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, "服务异常退出：", err)
		return 1
	}
	return 0
}

func runInit(args []string) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", config.DefaultConfigPath, "配置文件路径")
	dataDir := fs.String("data-dir", config.DefaultDataDir, "数据目录")
	webUser := fs.String("web-user", "smdadmin", "Web 登录账号")
	webPass := fs.String("web-pass", "", "Web 登录密码")
	webPort := fs.Int("web-port", 0, "Web 监听端口（0 表示保留原有配置）")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *webPort != 0 && !validPort(*webPort) {
		fmt.Fprintf(os.Stderr, "初始化失败：--web-port 必须在 1~65535 之间，当前为 %d\n", *webPort)
		return 1
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "初始化失败：", err)
		return 1
	}

	user := strings.TrimSpace(*webUser)
	pass := *webPass
	cur := cfg.Snapshot()
	explicitPass := pass != ""
	if !explicitPass {
		// 未显式指定则沿用既有口令。
		pass = cur.WebPass
	}
	if user == "" {
		user = cur.WebUser
	}
	if pass == "" {
		fmt.Fprintln(os.Stderr, "初始化失败：必须通过 --web-pass 指定 Web 登录密码")
		return 1
	}
	// 仅在显式设置新口令时校验强度：沿用既有口令的升级部署不应因为
	// 历史遗留的弱口令而直接失败（那样会让用户连升级都做不了）。
	if explicitPass {
		if err := config.ValidatePassword(user, pass); err != nil {
			fmt.Fprintln(os.Stderr, "初始化失败：", err)
			return 1
		}
	}

	next, err := cfg.Update(func(x *config.Config) {
		x.WebUser = user
		x.WebPass = pass
		if *webPort > 0 {
			x.WebPort = *webPort
		}
		x.DataDir = *dataDir
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "初始化失败：", err)
		return 1
	}

	// 提前初始化数据库，避免首次启动时目录不存在。
	st, err := store.Open(next.DataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "初始化失败：", err)
		return 1
	}
	st.Close()

	fmt.Println("配置初始化完成")
	fmt.Printf("  配置文件 : %s\n", cfg.Path())
	fmt.Printf("  数据目录 : %s\n", next.DataDir)
	fmt.Printf("  Web 端口 : %d\n", next.WebPort)
	fmt.Printf("  登录账号 : %s\n", next.WebUser)
	fmt.Printf("  识别码   : %s\n", next.UUID)
	return 0
}

// validPort 判断端口取值是否合法。
func validPort(p int) bool { return p >= 1 && p <= 65535 }

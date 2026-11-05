//go:build linux

package collect

import (
	"os"
	"strings"
	"time"
)

// readSysInfo 读取 Linux 的发行版本、内核版本与开机时刻。
func readSysInfo(now time.Time) (osName, kernel string, bootAt int64) {
	return readOSName(), readKernelRelease(), readBootAt(now)
}

// readOSName 读取发行版名称。多数发行版同时提供 /usr/lib/os-release
// 与指向它的 /etc/os-release，两个都试一遍以兼容各种定制镜像。
func readOSName() string {
	for _, path := range []string{"/etc/os-release", "/usr/lib/os-release"} {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		name := parseOSRelease(f)
		_ = f.Close()
		if name != "" {
			return name
		}
	}
	return ""
}

// readKernelRelease 读取内核版本号（等价于 uname -r），无需执行外部命令。
func readKernelRelease() string {
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// readBootAt 返回开机时刻（Unix 毫秒）。
//
// 优先 /proc/stat 中的 btime：它是墙钟时间，不受休眠 / 时钟漂移影响，
// 且不会每次读取都轻微变动（用「当前时间 - uptime」反推则会抖动）。
// 容器或老内核缺少 btime 时退回 /proc/uptime 反推。
func readBootAt(now time.Time) int64 {
	if f, err := os.Open("/proc/stat"); err == nil {
		sec := parseBtime(f)
		_ = f.Close()
		if sec > 0 {
			return sec * 1000
		}
	}
	if f, err := os.Open("/proc/uptime"); err == nil {
		sec := parseUptime(f)
		_ = f.Close()
		if sec > 0 {
			return now.Add(-time.Duration(sec * float64(time.Second))).UnixMilli()
		}
	}
	return 0
}

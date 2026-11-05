//go:build darwin

package collect

import (
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// readSysInfo 读取 macOS 的发行版本、内核版本与开机时刻。
// 仅用于开发机验证，生产目标是 Linux。
func readSysInfo(now time.Time) (osName, kernel string, bootAt int64) {
	return darwinOSName(), strings.TrimSpace(sysctlValue("kern.osrelease")), darwinBootAt()
}

// darwinOSName 组合成 "macOS 14.1" 这样的展示形式。
func darwinOSName() string {
	out, err := exec.Command("sw_vers", "-productVersion").Output()
	if err != nil {
		return runtime.GOOS
	}
	v := strings.TrimSpace(string(out))
	if v == "" {
		return runtime.GOOS
	}
	return "macOS " + v
}

func darwinBootAt() int64 {
	sec := parseSysctlBoottime(sysctlValue("kern.boottime"))
	if sec <= 0 {
		return 0
	}
	return sec * 1000
}

func sysctlValue(name string) string {
	out, err := exec.Command("sysctl", "-n", name).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

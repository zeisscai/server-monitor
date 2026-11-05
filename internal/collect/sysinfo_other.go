//go:build !linux && !darwin

package collect

import (
	"runtime"
	"time"
)

// readSysInfo 在不支持的平台上只能给出系统类型，内核与开机时刻留空。
func readSysInfo(now time.Time) (osName, kernel string, bootAt int64) {
	return runtime.GOOS, "", 0
}

// 系统信息采集：发行版本、内核版本、开机时刻。
//
// 这三项与 CPU / 内存这类「每次都变」的指标性质不同：
//
//   - 发行版本与内核版本只在系统升级或重启后变化；
//   - 开机时刻只在重启时变化，运行时长可以由它推算，不必每次都测。
//
// 因此它们被刻意排除在每次上报之外：采集器内部缓存后按需刷新，
// 上报端只在信息变化时携带一次（见 engine 中的 sysSendState）。
package collect

import (
	"strconv"
	"time"
)

// SysInfo 是服务器的低频系统信息。
type SysInfo struct {
	OS     string `json:"os"`      // 发行版本，如 "Ubuntu 22.04.3 LTS"
	Kernel string `json:"kernel"`  // 内核版本，如 "5.15.0-91-generic"
	BootAt int64  `json:"boot_at"` // 开机时刻（Unix 毫秒），0 表示未知
}

// UptimeAt 返回 now 时刻已运行的时长；开机时刻未知时返回 0。
func (s SysInfo) UptimeAt(now time.Time) time.Duration {
	if s.BootAt <= 0 {
		return 0
	}
	if d := now.Sub(time.UnixMilli(s.BootAt)); d > 0 {
		return d
	}
	return 0
}

// Key 返回用于判断「信息是否变化」的指纹。
//
// 只要它没变，上报时就没必要重复携带这组字段。
func (s SysInfo) Key() string {
	return s.OS + "\x00" + s.Kernel + "\x00" + strconv.FormatInt(s.BootAt, 10)
}

// 缓存刷新间隔。发行版 / 内核只会在升级或重启后变化，10 分钟重读一次
// （两次几十字节的读文件）既不会漏掉变化，开销也可以忽略。
const sysInfoTTL = 10 * time.Minute

// 读取失败（例如精简系统没有 /etc/os-release）时的重试间隔，
// 避免每 2 秒都去打开一个不存在的文件。
const sysInfoRetry = time.Minute

// sysCache 缓存系统信息。
//
// 采集循环是单个 goroutine 顺序调用 Read，因此这里不需要加锁。
type sysCache struct {
	info   SysInfo
	readAt time.Time
}

// get 返回当前系统信息，必要时重新读取。
func (c *sysCache) get(now time.Time) SysInfo {
	if !c.readAt.IsZero() {
		ttl := sysInfoTTL
		if c.info.OS == "" {
			ttl = sysInfoRetry
		}
		if now.Sub(c.readAt) < ttl {
			return c.info
		}
	}

	osName, kernel, bootAt := readSysInfo(now)
	if osName != "" {
		c.info.OS = osName
	}
	if kernel != "" {
		c.info.Kernel = kernel
	}
	// 开机时刻只在第一次成功读取时采纳：之后每次用「当前时间 - uptime」
	// 反推会有几秒抖动，会让「信息是否变化」的判断一直成立。
	if c.info.BootAt == 0 {
		c.info.BootAt = bootAt
	}
	c.readAt = now
	return c.info
}

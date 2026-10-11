// Package security 提供安全相关的公共小工具：常量时间比较、安全审计日志
// 与客户端地址解析。
//
// 关于日志：本程序刻意不做常规运行日志，但登录失败、限流触发、鉴权失败等
// 安全事件必须可追溯，因此这里提供唯一的最小化日志出口，全部写往 stderr，
// 由 systemd / journald 收集。任何情况下都不得把密码或 API-Key 明文写进日志。
package security

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Equal 以常量时间比较两个字符串是否相等。
//
// 先各自做一次 SHA-256 再比较，使参与比较的数据长度恒为 32 字节。
// 这一步是必要的：subtle.ConstantTimeCompare 在两侧长度不同时会立即返回，
// 直接比较明文会泄露「已知前缀长度」这类信息。
func Equal(a, b string) bool {
	x := sha256.Sum256([]byte(a))
	y := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(x[:], y[:]) == 1
}

// ClientIP 返回请求的客户端 IP（不含端口）。
//
// 刻意不读取 X-Forwarded-For：该头部可被客户端随意伪造，一旦用于限流，
// 等于把「绕过限流」的能力直接交给攻击者。若日后部署在反向代理之后，
// 应改为「只信任显式配置的代理地址」再解析该头部。
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

const (
	auditMaxNameLen  = 32
	auditMaxValueLen = 96
	// eventEveryTTL 是 EventEvery 内部记录的空闲回收阈值。
	eventEveryTTL  = time.Hour
	eventEveryGate = 1024
)

var auditMu sync.Mutex

// Event 输出一条安全审计事件到 stderr。
//
// kv 为成对的「键, 值」，例如 Event("login_failure", "ip", ip, "user", user)。
// 所有字段都会剔除控制字符并截断，避免攻击者通过可控字段（如登录失败时
// 提交的用户名）注入换行符来伪造日志行、污染审计记录。
func Event(name string, kv ...any) {
	var b strings.Builder
	b.WriteString("[安全] ")
	b.WriteString(time.Now().Format("2006-01-02 15:04:05"))
	b.WriteString(" ")
	b.WriteString(sanitize(name, auditMaxNameLen))

	for i := 0; i+1 < len(kv); i += 2 {
		key := sanitize(fmt.Sprint(kv[i]), auditMaxNameLen)
		if key == "" {
			continue
		}
		b.WriteString(" ")
		b.WriteString(key)
		b.WriteString("=")
		b.WriteString(strconv.Quote(sanitize(fmt.Sprint(kv[i+1]), auditMaxValueLen)))
	}
	b.WriteString("\n")

	auditMu.Lock()
	_, _ = os.Stderr.WriteString(b.String())
	auditMu.Unlock()
}

var (
	everyMu   sync.Mutex
	everyLast = map[string]time.Time{}
)

// EventEvery 按 key 限频输出安全事件：同一 key 在 every 内最多输出一次，
// 返回本次是否真的输出了。
//
// 用于「未认证请求即可任意触发」的事件（例如接收端 API-Key 校验失败）：
// 若不限频，攻击者可以用海量请求把日志写成洪水，反过来打满磁盘、
// 让审计记录失去价值。
func EventEvery(key string, every time.Duration, name string, kv ...any) bool {
	if every <= 0 {
		every = time.Minute
	}
	now := time.Now()

	everyMu.Lock()
	if last, ok := everyLast[key]; ok && now.Sub(last) < every {
		everyMu.Unlock()
		return false
	}
	everyLast[key] = now
	if len(everyLast) > eventEveryGate {
		for k, t := range everyLast {
			if now.Sub(t) > eventEveryTTL {
				delete(everyLast, k)
			}
		}
	}
	everyMu.Unlock()

	Event(name, kv...)
	return true
}

// sanitize 去除控制字符（含换行、回车、制表符）并按字符数截断。
func sanitize(s string, max int) string {
	cleaned := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)

	rs := []rune(cleaned)
	if len(rs) > max {
		rs = rs[:max]
	}
	return string(rs)
}

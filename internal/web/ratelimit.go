package web

import (
	"sync"
	"time"
)

// 登录失败限流参数。
//
// 策略：按 IP 指数退避 + 全局滑动窗口限流，刻意不做硬锁定。
//
// 依据：被限流拦下的请求不计数，惩罚会随时间自然衰减，因此合法管理员
// 不会因为别人（或自己）输错密码而被永久锁在门外；同时单个 IP 的爆破
// 速率被压到极低，分布式爆破则被全局窗口约束。全局窗口是「拒绝所有人
// 一段时间」而非「封锁某个账号」，最长也就一个窗口，属于可接受代价。
const (
	// loginFreeAttempts：前若干次失败只提示错误、不触发退避，用于容忍手误。
	loginFreeAttempts = 3
	// 退避时长自 loginBaseBackoff 起逐次翻倍，上限 loginMaxBackoff。
	loginBaseBackoff = 2 * time.Second
	loginMaxBackoff  = 15 * time.Minute

	// 全局滑动窗口：loginGlobalWindow 内失败达到 loginGlobalMaxFails 次后，
	// 暂时拒绝所有登录尝试，直到窗口内最早的失败滚出窗口。
	loginGlobalWindow   = time.Minute
	loginGlobalMaxFails = 20

	// 空闲超过 loginStateTTL 的 IP 记录会被回收，避免 map 无限增长。
	loginStateTTL  = time.Hour
	loginPruneGate = 1024

	// loginFailureDelay 是凭据错误时的固定响应延时，用于抹平响应时间差异、
	// 进一步降低被允许的那几次尝试的爆破效率。
	loginFailureDelay = 400 * time.Millisecond
)

// limitDecision 是一次限流判定的结果。
type limitDecision struct {
	Allowed    bool
	RetryAfter time.Duration
	Scope      string // "ip" 或 "global"，仅在被拒绝时有意义
}

type ipFailures struct {
	failures    int
	bannedUntil time.Time
	lastSeen    time.Time
}

// loginLimiter 是并发安全的登录限流器：按 IP 退避并叠加全局滑动窗口。
type loginLimiter struct {
	mu     sync.Mutex
	ips    map[string]*ipFailures
	global []time.Time      // 窗口内的失败时刻，按时间升序
	now    func() time.Time // 可注入，便于测试
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{ips: make(map[string]*ipFailures), now: time.Now}
}

// allow 判断本次登录尝试是否放行。此方法不产生任何副作用。
func (l *loginLimiter) allow(ip string) limitDecision {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.pruneGlobal(now)

	// 全局窗口优先判定：分布式爆破时所有来源一起被挡。
	if len(l.global) >= loginGlobalMaxFails {
		wait := l.global[0].Add(loginGlobalWindow).Sub(now)
		if wait < time.Second {
			wait = time.Second
		}
		return limitDecision{RetryAfter: wait, Scope: "global"}
	}

	if st := l.ips[ip]; st != nil && now.Before(st.bannedUntil) {
		wait := st.bannedUntil.Sub(now)
		if wait < time.Second {
			wait = time.Second
		}
		return limitDecision{RetryAfter: wait, Scope: "ip"}
	}
	return limitDecision{Allowed: true}
}

// fail 记录一次凭据错误，返回该 IP 的累计失败次数与本次新开启的退避时长
// （0 表示尚未进入退避）。
func (l *loginLimiter) fail(ip string) (int, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.global = append(l.global, now)
	l.pruneGlobal(now)

	st := l.ips[ip]
	if st == nil || now.Sub(st.lastSeen) > loginStateTTL {
		st = &ipFailures{}
		l.ips[ip] = st
	}
	st.failures++
	st.lastSeen = now

	var banFor time.Duration
	if st.failures > loginFreeAttempts {
		banFor = backoffFor(st.failures)
		if until := now.Add(banFor); until.After(st.bannedUntil) {
			st.bannedUntil = until
		}
	}
	l.pruneIPs(now)
	return st.failures, banFor
}

// success 在登录成功后清除该 IP 的失败状态。
func (l *loginLimiter) success(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.ips, ip)
}

// backoffFor 计算累计第 failures 次失败应退避的时长：基数逐次翻倍至上限。
//
// 用循环而非移位，避免失败次数持续增长时移位溢出。
func backoffFor(failures int) time.Duration {
	d := loginBaseBackoff
	for i := loginFreeAttempts + 1; i < failures; i++ {
		if d >= loginMaxBackoff {
			break
		}
		d *= 2
	}
	if d > loginMaxBackoff {
		d = loginMaxBackoff
	}
	return d
}

// pruneGlobal 丢弃已滚出滑动窗口的失败记录。
func (l *loginLimiter) pruneGlobal(now time.Time) {
	cut := now.Add(-loginGlobalWindow)
	i := 0
	for i < len(l.global) && !l.global[i].After(cut) {
		i++
	}
	if i > 0 {
		l.global = append(l.global[:0], l.global[i:]...)
	}
}

// pruneIPs 在条目数超过阈值时回收长期空闲的 IP 记录。
func (l *loginLimiter) pruneIPs(now time.Time) {
	if len(l.ips) <= loginPruneGate {
		return
	}
	for k, st := range l.ips {
		if now.Sub(st.lastSeen) > loginStateTTL {
			delete(l.ips, k)
		}
	}
}

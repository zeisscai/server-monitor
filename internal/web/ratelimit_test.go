package web

import (
	"testing"
	"time"
)

// newTestLimiter 返回一个时钟可注入的限流器。
func newTestLimiter() (*loginLimiter, *time.Time) {
	l := newLoginLimiter()
	cur := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return cur }
	return l, &cur
}

func TestLimiterAllowsWithinFreeAttempts(t *testing.T) {
	l, _ := newTestLimiter()
	const ip = "10.0.0.1"

	for i := 1; i <= loginFreeAttempts; i++ {
		if d := l.allow(ip); !d.Allowed {
			t.Fatalf("第 %d 次尝试在免罚次数内应放行，实际被拒（scope=%s）", i, d.Scope)
		}
		failures, banFor := l.fail(ip)
		if failures != i {
			t.Fatalf("失败计数应为 %d，实际 %d", i, failures)
		}
		if banFor != 0 {
			t.Fatalf("第 %d 次失败不应触发退避，实际退避 %v", i, banFor)
		}
	}
}

func TestLimiterBackoffGrowsExponentially(t *testing.T) {
	l, cur := newTestLimiter()
	const ip = "10.0.0.1"

	// 先耗尽免罚次数。
	for i := 0; i < loginFreeAttempts; i++ {
		l.fail(ip)
	}

	wantBackoffs := []time.Duration{
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		32 * time.Second,
	}
	for i, want := range wantBackoffs {
		_, banFor := l.fail(ip)
		if banFor != want {
			t.Fatalf("第 %d 次退避应为 %v，实际 %v", i+1, want, banFor)
		}
		// 退避期内应被拒绝。
		d := l.allow(ip)
		if d.Allowed {
			t.Fatalf("退避期内应被拒绝，实际放行")
		}
		if d.Scope != "ip" {
			t.Fatalf("退避期内 scope 应为 ip，实际 %q", d.Scope)
		}
		if d.RetryAfter <= 0 || d.RetryAfter > want {
			t.Fatalf("重试等待时长应在 (0, %v] 内，实际 %v", want, d.RetryAfter)
		}
		// 时间推进到退避结束之后应恢复放行。
		*cur = cur.Add(want + time.Second)
		if d := l.allow(ip); !d.Allowed {
			t.Fatalf("退避结束后应放行，实际被拒（scope=%s）", d.Scope)
		}
	}
}

func TestLimiterBackoffCappedAtMax(t *testing.T) {
	l, _ := newTestLimiter()
	const ip = "10.0.0.1"

	for i := 0; i < loginFreeAttempts; i++ {
		l.fail(ip)
	}
	// 连续失败足够多次后，退避时长应被上限截断而不是无限增长。
	var last time.Duration
	for i := 0; i < 40; i++ {
		_, last = l.fail(ip)
	}
	if last != loginMaxBackoff {
		t.Fatalf("退避时长应收敛到上限 %v，实际 %v", loginMaxBackoff, last)
	}
}

// 登录成功必须清除该 IP 的失败状态，否则合法用户会被自己之前的失误拖累。
func TestLimiterSuccessResetsState(t *testing.T) {
	l, _ := newTestLimiter()
	const ip = "10.0.0.1"

	for i := 0; i < loginFreeAttempts+2; i++ {
		l.fail(ip)
	}
	if d := l.allow(ip); d.Allowed {
		t.Fatal("多次失败后应处于退避中")
	}

	l.success(ip)

	if d := l.allow(ip); !d.Allowed {
		t.Fatal("登录成功后应恢复放行")
	}
	// 计数也应归零：下次失败重新从 1 开始。
	failures, banFor := l.fail(ip)
	if failures != 1 || banFor != 0 {
		t.Fatalf("成功后失败计数应从 1 重新开始，实际 failures=%d banFor=%v", failures, banFor)
	}
}

// 退避只针对触发的那个 IP，不应牵连其它来源。
func TestLimiterIsolatesPerIP(t *testing.T) {
	l, _ := newTestLimiter()

	for i := 0; i < loginFreeAttempts+2; i++ {
		l.fail("10.0.0.1")
	}
	if d := l.allow("10.0.0.1"); d.Allowed {
		t.Fatal("触发退避的 IP 应被拒绝")
	}
	if d := l.allow("10.0.0.2"); !d.Allowed {
		t.Fatal("其它 IP 不应被牵连")
	}
}

// 全局滑动窗口：分布式爆破（不断更换 IP）必须被整体挡住，且窗口过后自动恢复。
func TestLimiterGlobalWindow(t *testing.T) {
	l, cur := newTestLimiter()

	// 用不同 IP 打满全局窗口（每个 IP 只失败一次，绕开单 IP 退避）。
	for i := 0; i < loginGlobalMaxFails; i++ {
		ip := "10.0." + string(rune('a'+i/26)) + "." + string(rune('a'+i%26))
		l.fail(ip)
	}

	// 一个全新的 IP 也应被全局窗口拒绝。
	d := l.allow("192.168.99.99")
	if d.Allowed {
		t.Fatal("全局窗口用尽后，新 IP 也应被拒绝")
	}
	if d.Scope != "global" {
		t.Fatalf("scope 应为 global，实际 %q", d.Scope)
	}
	if d.RetryAfter <= 0 || d.RetryAfter > loginGlobalWindow {
		t.Fatalf("全局重试等待时长应在 (0, %v] 内，实际 %v", loginGlobalWindow, d.RetryAfter)
	}

	// 窗口内最早的失败滚出后，应恢复放行（不做硬锁定）。
	*cur = cur.Add(loginGlobalWindow + time.Second)
	if d := l.allow("192.168.99.99"); !d.Allowed {
		t.Fatalf("窗口过后应自动恢复，实际仍被拒（scope=%s）", d.Scope)
	}
}

// 被限流拦下的请求不计数，因此惩罚必须能自然衰减，而不是每次访问都续期。
func TestLimiterRejectedRequestsDoNotExtendBan(t *testing.T) {
	l, cur := newTestLimiter()
	const ip = "10.0.0.1"

	for i := 0; i < loginFreeAttempts; i++ {
		l.fail(ip)
	}
	_, banFor := l.fail(ip) // 触发首次退避
	if banFor == 0 {
		t.Fatal("应触发退避")
	}

	// 在退避期内反复「尝试」，若这些尝试续期，退避将永不结束。
	for i := 0; i < 100; i++ {
		if d := l.allow(ip); d.Allowed {
			t.Fatal("退避期内不应放行")
		}
	}

	*cur = cur.Add(banFor + time.Second)
	if d := l.allow(ip); !d.Allowed {
		t.Fatal("退避期结束后应放行（被拒请求不应续期）")
	}
}

func TestBackoffForMonotonicAndCapped(t *testing.T) {
	prev := time.Duration(0)
	for f := loginFreeAttempts + 1; f < loginFreeAttempts+30; f++ {
		got := backoffFor(f)
		if got < prev {
			t.Fatalf("failures=%d 时退避时长不应回退：%v < %v", f, got, prev)
		}
		if got > loginMaxBackoff {
			t.Fatalf("failures=%d 时退避时长超过上限：%v", f, got)
		}
		prev = got
	}
	if backoffFor(loginFreeAttempts+1) != loginBaseBackoff {
		t.Fatalf("首次退避应为基数 %v，实际 %v", loginBaseBackoff, backoffFor(loginFreeAttempts+1))
	}
	// 极大的失败次数不应因移位/溢出产生异常值。
	if got := backoffFor(100000); got != loginMaxBackoff {
		t.Fatalf("极大失败次数应收敛到上限，实际 %v", got)
	}
}

// 过期条目应被回收，避免攻击者用海量来源 IP 把内存撑爆。
func TestLimiterPrunesIdleEntries(t *testing.T) {
	l, cur := newTestLimiter()

	for i := 0; i < loginPruneGate+1; i++ {
		l.fail("10.0." + string(rune('a'+i%26)) + "." + string(rune('a'+i/26)))
	}
	if len(l.ips) <= loginPruneGate {
		t.Fatalf("预置条件不成立：应超过回收阈值，实际 %d", len(l.ips))
	}

	// 让所有条目变旧，再触发一次失败以驱动回收。
	*cur = cur.Add(loginStateTTL + time.Minute)
	l.fail("172.16.0.1")

	for k, st := range l.ips {
		if st.lastSeen.Before(cur.Add(-loginStateTTL)) && k != "172.16.0.1" {
			t.Fatalf("空闲条目 %q 未被回收", k)
		}
	}
}

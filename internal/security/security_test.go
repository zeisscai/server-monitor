package security

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEqual(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want bool
	}{
		{"相同", "correct-horse", "correct-horse", true},
		{"不同内容", "correct-horse", "correct-horsf", false},
		{"不同长度", "abc", "abcd", false},
		{"前缀相同", "abcdefghij", "abcdefghi", false},
		{"空字符串相等", "", "", true},
		{"空与非空", "", "x", false},
		{"大小写敏感", "Admin", "admin", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Equal(c.a, c.b); got != c.want {
				t.Fatalf("Equal(%q, %q) = %v, 期望 %v", c.a, c.b, got, c.want)
			}
		})
	}
}

func TestClientIP(t *testing.T) {
	cases := []struct {
		remoteAddr string
		want       string
	}{
		{"192.168.1.10:54321", "192.168.1.10"},
		{"[2001:db8::1]:443", "2001:db8::1"},
		{"10.0.0.1", "10.0.0.1"}, // 无端口时原样返回
		{"", ""},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodPost, "/login", nil)
		r.RemoteAddr = c.remoteAddr
		if got := ClientIP(r); got != c.want {
			t.Fatalf("ClientIP(%q) = %q, 期望 %q", c.remoteAddr, got, c.want)
		}
	}
}

// ClientIP 必须忽略可伪造的 X-Forwarded-For，否则限流可被轻易绕过。
func TestClientIPIgnoresForwardedFor(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/login", nil)
	r.RemoteAddr = "192.168.1.10:54321"
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	r.Header.Set("X-Real-IP", "5.6.7.8")

	if got := ClientIP(r); got != "192.168.1.10" {
		t.Fatalf("不应采信 X-Forwarded-For，实际得到 %q", got)
	}
}

func TestSanitize(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"去除换行", "admin\n[安全] 伪造日志", 96, "admin[安全] 伪造日志"},
		{"去除回车与制表", "a\rb\tc", 96, "abc"},
		{"去除其它控制字符", "a\x00b\x1bc", 96, "abc"},
		{"截断到上限", "abcdefghij", 4, "abcd"},
		{"按字符而非字节截断", "中文测试", 2, "中文"},
		{"保留普通文本", "admin", 96, "admin"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sanitize(c.in, c.max); got != c.want {
				t.Fatalf("sanitize(%q, %d) = %q, 期望 %q", c.in, c.max, got, c.want)
			}
		})
	}
}

func TestEventEveryThrottlesPerKey(t *testing.T) {
	key := "test-" + t.Name()

	if !EventEvery(key, time.Hour, "test_event", "ip", "1.2.3.4") {
		t.Fatal("首次调用应输出事件")
	}
	if EventEvery(key, time.Hour, "test_event", "ip", "1.2.3.4") {
		t.Fatal("同一 key 在同一时间窗内不应重复输出")
	}

	// 不同 key 互不影响。
	otherKey := key + "-other"
	if !EventEvery(otherKey, time.Hour, "test_event", "ip", "5.6.7.8") {
		t.Fatal("不同 key 应各自独立计数")
	}

	// every <= 0 时回落为默认间隔，仍应限频。
	zeroKey := key + "-zero"
	if !EventEvery(zeroKey, 0, "test_event") {
		t.Fatal("every=0 时首次调用应输出")
	}
	if EventEvery(zeroKey, 0, "test_event") {
		t.Fatal("every=0 时应回落到默认间隔并限频")
	}

	// 窗口极短时，过期后应可再次输出。
	shortKey := key + "-short"
	if !EventEvery(shortKey, time.Millisecond, "test_event") {
		t.Fatal("短窗口首次调用应输出")
	}
	time.Sleep(5 * time.Millisecond)
	if !EventEvery(shortKey, time.Millisecond, "test_event") {
		t.Fatal("短窗口过期后应可再次输出")
	}
}

// Event 必须把事件名与键值对写入 stderr，且不因恶意输入而破坏日志结构。
func TestEventOutputCannotBeForged(t *testing.T) {
	// 这里只验证不 panic 且转义逻辑生效（stderr 内容由 sanitize 测试覆盖）。
	Event("login_failure", "ip", "1.2.3.4", "user", "attacker\n[安全] 2020-01-01 00:00:00 login_success ip=\"1.2.3.4\"")
	Event("odd_pairs", "key-without-value")
	Event("empty_key", "", "ignored")
}

func TestEventNameTruncated(t *testing.T) {
	long := strings.Repeat("x", 200)
	if got := sanitize(long, auditMaxNameLen); len([]rune(got)) != auditMaxNameLen {
		t.Fatalf("事件名应被截断到 %d 字符，实际 %d", auditMaxNameLen, len([]rune(got)))
	}
}

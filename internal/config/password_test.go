package config

import (
	"strings"
	"testing"
)

func TestValidatePasswordAccepts(t *testing.T) {
	cases := []struct {
		name string
		user string
		pass string
	}{
		{"随机强口令", "smdadmin", "Xk7#pQ2mLz"},
		{"部署脚本生成的 16 位十六进制", "smdadmin", "a3f91c07be24d5e8"},
		{"恰好达到最小长度", "smdadmin", "abcd1234"},
		{"含中文的长口令", "smdadmin", "监控面板口令一二三"},
		{"长口令短语", "smdadmin", "correct-horse-battery-staple"},
		{"含账号但不是账号本身", "smdadmin", "smdadmin-2026-ok"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := ValidatePassword(c.user, c.pass); err != nil {
				t.Fatalf("ValidatePassword(%q, %q) 应被接受，实际报错: %v", c.user, c.pass, err)
			}
		})
	}
}

func TestValidatePasswordRejects(t *testing.T) {
	cases := []struct {
		name string
		user string
		pass string
		want string // 期望错误信息包含的子串
	}{
		{"单个字符", "smdadmin", "1", "至少"},
		{"空口令", "smdadmin", "", "至少"},
		{"7 位不足下限", "smdadmin", "abcd123", "至少"},
		// 7 个中文字符 = 21 字节但只有 7 个字符；这条用来确保按「字符」而非
		// 「字节」计数，否则会被误判为达标。
		{"按字符计长度（7 个中文字符）", "smdadmin", "监控面板口令密", "至少"},
		{"常见弱口令", "smdadmin", "password", "常见弱口令"},
		{"常见弱口令大写变体", "smdadmin", "PASSWORD", "常见弱口令"},
		{"数字序列弱口令", "smdadmin", "12345678", "常见弱口令"},
		{"项目名当口令", "smdadmin", "servermonitor", "常见弱口令"},
		{"与账号相同", "webmaster01", "webmaster01", "不能与登录账号相同"},
		{"与账号相同（忽略大小写）", "WebMaster01", "webmaster01", "不能与登录账号相同"},
		{"同一字符重复", "smdadmin", "aaaaaaaa", "同一个字符的重复"},
		{"同一中文字符重复", "smdadmin", "密密密密密密密密", "同一个字符的重复"},
		{"超过长度上限", "smdadmin", strings.Repeat("a", MaxPasswordLen+1), "不能超过"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidatePassword(c.user, c.pass)
			if err == nil {
				t.Fatalf("ValidatePassword(%q, %q) 应被拒绝，实际通过", c.user, c.pass)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("错误信息应包含 %q，实际 %q", c.want, err.Error())
			}
		})
	}
}

// 恰好等于上限的口令应当被接受（边界值）。
func TestValidatePasswordAtMaxLength(t *testing.T) {
	pass := strings.Repeat("aB3", MaxPasswordLen/3) + strings.Repeat("x", MaxPasswordLen%3)
	if len([]rune(pass)) != MaxPasswordLen {
		t.Fatalf("测试数据长度应为 %d，实际 %d", MaxPasswordLen, len([]rune(pass)))
	}
	if err := ValidatePassword("smdadmin", pass); err != nil {
		t.Fatalf("恰好达到上限的口令应被接受，实际报错: %v", err)
	}
}

func TestAllSameRune(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"aaaaaaaa", true},
		{"密密密密", true},
		{"a", true},
		{"", false},
		{"aaab", false},
		{"ab", false},
	}
	for _, c := range cases {
		if got := allSameRune(c.in); got != c.want {
			t.Fatalf("allSameRune(%q) = %v，期望 %v", c.in, got, c.want)
		}
	}
}

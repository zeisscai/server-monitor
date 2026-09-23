package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewUUIDFormat(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		u := NewUUID()
		if len(u) != 36 {
			t.Fatalf("UUID 长度应为 36，实际 %d: %s", len(u), u)
		}
		if !IsUUID(u) {
			t.Fatalf("UUID 格式不合法: %s", u)
		}
		if u[14] != '4' {
			t.Fatalf("应为 UUID v4，实际: %s", u)
		}
		if seen[u] {
			t.Fatalf("UUID 重复: %s", u)
		}
		seen[u] = true
	}
	if IsUUID("not-a-uuid") || IsUUID("") || IsUUID("b6f2f65e43ec45c780e851cede6b2d23") {
		t.Fatal("IsUUID 对非法输入返回了 true")
	}
}

func TestNewAPIKeyLength(t *testing.T) {
	for i := 0; i < 100; i++ {
		k := NewAPIKey()
		if len(k) != 18 {
			t.Fatalf("API-Key 长度应为 18，实际 %d: %s", len(k), k)
		}
		for _, c := range k {
			if !strings.ContainsRune(apiKeyAlphabet, c) {
				t.Fatalf("API-Key 含非法字符 %q: %s", c, k)
			}
		}
	}
}

func TestEffectiveInterval(t *testing.T) {
	// 上报关闭时固定 5 秒，与 report_interval 无关
	c := Config{ReportEnabled: false, ReportInterval: 2}
	if got := c.EffectiveInterval(); got != 5*time.Second {
		t.Fatalf("上报关闭时应为 5s，实际 %v", got)
	}
	// 上报开启时使用上报频率
	for _, v := range AllowedIntervals {
		c := Config{ReportEnabled: true, ReportInterval: v}
		if got := c.EffectiveInterval(); got != time.Duration(v)*time.Second {
			t.Fatalf("上报频率 %d 时实际 %v", v, got)
		}
	}
	// 非法频率回落到默认 5 秒
	c = Config{ReportEnabled: true, ReportInterval: 7}
	if got := c.EffectiveInterval(); got != 5*time.Second {
		t.Fatalf("非法频率应回落 5s，实际 %v", got)
	}
}

func TestNormalizeDefaults(t *testing.T) {
	c := Config{}.Normalize()
	if !IsUUID(c.UUID) {
		t.Fatalf("应生成 UUID: %q", c.UUID)
	}
	if c.WebPort != DefaultWebPort {
		t.Fatalf("默认 Web 端口应为 %d，实际 %d", DefaultWebPort, c.WebPort)
	}
	if c.ReceiveEnabled || c.ReportEnabled {
		t.Fatal("接收与上报默认应关闭")
	}
	if c.ReceiveAPIKey != "" {
		t.Fatal("接收关闭时不应保留 API-Key")
	}
	if c.ReportInterval != DefaultReportInterval {
		t.Fatalf("默认上报频率应为 %d，实际 %d", DefaultReportInterval, c.ReportInterval)
	}
	if c.SessionSecret == "" {
		t.Fatal("应生成会话密钥")
	}
}

func TestNormalizeGeneratesKeyWhenEnabled(t *testing.T) {
	c := Config{ReceiveEnabled: true}.Normalize()
	if len(c.ReceiveAPIKey) != 18 {
		t.Fatalf("开启接收应生成 18 位 Key，实际 %q", c.ReceiveAPIKey)
	}
	// 关闭后清空
	c.ReceiveEnabled = false
	if got := c.Normalize().ReceiveAPIKey; got != "" {
		t.Fatalf("关闭接收后 Key 应清空，实际 %q", got)
	}
}

func TestValidate(t *testing.T) {
	base := Config{}.Normalize()

	bad := base
	bad.ReceiveEnabled = true
	bad.ReceivePort = bad.WebPort
	if err := bad.Validate(); err == nil {
		t.Fatal("接收端口与 Web 端口相同时应校验失败")
	}

	bad = base
	bad.ReportEnabled = true
	if err := bad.Validate(); err == nil {
		t.Fatal("开启上报但未填目标 IP 时应校验失败")
	}

	bad = base
	bad.ReportEnabled = true
	bad.ReportTargetIP = "192.168.1.10"
	bad.ReportTargetPort = 9100
	bad.ReportInterval = 3
	if err := bad.Validate(); err == nil {
		t.Fatal("上报频率非 2/5/10 时应校验失败")
	}

	bad = base
	bad.ReportEnabled = true
	bad.ReportTargetIP = "192.168.1.10"
	bad.ReportTargetPort = 9100
	bad.ReportAPIKey = ""
	if err := bad.Validate(); err == nil {
		t.Fatal("开启上报但未填 API-Key 时应校验失败")
	}

	ok := base
	ok.ReportEnabled = true
	ok.ReportTargetIP = "192.168.1.10"
	ok.ReportTargetPort = 9100
	ok.ReportAPIKey = NewAPIKey()
	if err := ok.Validate(); err != nil {
		t.Fatalf("合法配置不应报错: %v", err)
	}
}

func TestLoadCreatesFileAndKeepsUUID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "config.json")

	m, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if m.Existed() {
		t.Fatal("首次加载不应标记为已存在")
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("配置文件未生成: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("配置文件权限应为 600，实际 %o", perm)
	}
	first := m.Snapshot().UUID
	if !IsUUID(first) {
		t.Fatalf("未生成合法 UUID: %q", first)
	}

	m2, err := Load(path)
	if err != nil {
		t.Fatalf("二次 Load 失败: %v", err)
	}
	if !m2.Existed() {
		t.Fatal("二次加载应标记为已存在")
	}
	if m2.Snapshot().UUID != first {
		t.Fatal("重新加载后 UUID 发生了变化")
	}
}

func TestUpdatePersistsAndNotifies(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	m, err := Load(path)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}

	called := 0
	var got Config
	m.OnChange(func(c Config) {
		called++
		got = c
		// 回调中再次读取配置，验证不会死锁
		_ = m.Snapshot().UUID
	})

	// 开启接收会自动生成 Key
	next, err := m.Update(func(c *Config) {
		c.WebUser = "tester"
		c.WebPass = "pw"
		c.ReceiveEnabled = true
		c.ReceivePort = 19100
	})
	if err != nil {
		t.Fatalf("Update 失败: %v", err)
	}
	if called != 1 {
		t.Fatalf("变更回调应被调用 1 次，实际 %d", called)
	}
	if got.ReceiveAPIKey != next.ReceiveAPIKey || len(next.ReceiveAPIKey) != 18 {
		t.Fatalf("回调未拿到新配置或 Key 未生成: %q", next.ReceiveAPIKey)
	}
	if m.Snapshot().ReceiveAPIKey != next.ReceiveAPIKey {
		t.Fatal("内存中的配置未更新")
	}

	// 校验失败时不应落盘、不应回调
	_, err = m.Update(func(c *Config) {
		c.ReportEnabled = true
		c.ReportTargetIP = ""
	})
	if err == nil {
		t.Fatal("非法配置应返回错误")
	}
	if called != 1 {
		t.Fatal("校验失败不应触发变更回调")
	}

	m3, err := Load(path)
	if err != nil {
		t.Fatalf("重新加载失败: %v", err)
	}
	if m3.Snapshot().WebUser != "tester" || !m3.Snapshot().ReceiveEnabled {
		t.Fatal("配置未持久化")
	}
}

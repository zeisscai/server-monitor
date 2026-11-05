package collect

import (
	"strings"
	"testing"
	"time"
)

func TestParseOSRelease(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "标准格式",
			in: "NAME=\"Ubuntu\"\nVERSION=\"22.04.3 LTS (Jammy Jellyfish)\"\n" +
				"PRETTY_NAME=\"Ubuntu 22.04.3 LTS\"\nVERSION_ID=\"22.04\"\n",
			want: "Ubuntu 22.04.3 LTS",
		},
		{
			name: "单引号",
			in:   "PRETTY_NAME='Debian GNU/Linux 12 (bookworm)'\n",
			want: "Debian GNU/Linux 12 (bookworm)",
		},
		{
			name: "没有 PRETTY_NAME 时拼 NAME + VERSION_ID",
			in:   "NAME=\"Alpine Linux\"\nVERSION_ID=3.19.1\n",
			want: "Alpine Linux 3.19.1",
		},
		{
			name: "只有 NAME",
			in:   "NAME=CustomLinux\n",
			want: "CustomLinux",
		},
		{
			name: "注释与空行会被忽略",
			in:   "# 注释\n\nPRETTY_NAME=\"CentOS Stream 9\"\n",
			want: "CentOS Stream 9",
		},
		{
			name: "空内容",
			in:   "",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseOSRelease(strings.NewReader(tc.in)); got != tc.want {
				t.Fatalf("parseOSRelease(%q) = %q，期望 %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseBtime(t *testing.T) {
	in := `cpu  1 2 3 4
cpu0 1 2 3 4
intr 12345
btime 1699999999
processes 8888
`
	if got := parseBtime(strings.NewReader(in)); got != 1699999999 {
		t.Fatalf("btime 解析错误：%d", got)
	}
	// 缺少 btime 行（部分容器）时应返回 0，由上层退回 /proc/uptime。
	if got := parseBtime(strings.NewReader("cpu 1 2 3\n")); got != 0 {
		t.Fatalf("无 btime 时应返回 0，实际 %d", got)
	}
	if got := parseBtime(strings.NewReader("btime abc\n")); got != 0 {
		t.Fatalf("非法 btime 应返回 0，实际 %d", got)
	}
}

func TestParseUptime(t *testing.T) {
	if got := parseUptime(strings.NewReader("12345.67 98765.43\n")); got != 12345.67 {
		t.Fatalf("uptime 解析错误：%v", got)
	}
	if got := parseUptime(strings.NewReader("")); got != 0 {
		t.Fatalf("空内容应返回 0，实际 %v", got)
	}
	if got := parseUptime(strings.NewReader("0.00 0.00\n")); got != 0 {
		t.Fatalf("0 应视为无效，实际 %v", got)
	}
}

func TestParseSysctlBoottime(t *testing.T) {
	in := "{ sec = 1699999999, usec = 123456 } Thu Nov  9 10:00:00 2023\n"
	if got := parseSysctlBoottime(in); got != 1699999999 {
		t.Fatalf("boottime 解析错误：%d", got)
	}
	if got := parseSysctlBoottime("{ sec = 0, usec = 0 }"); got != 0 {
		t.Fatalf("0 应视为无效，实际 %d", got)
	}
	if got := parseSysctlBoottime("garbage"); got != 0 {
		t.Fatalf("无法解析时应返回 0，实际 %d", got)
	}
}

func TestSysInfoKeyAndUptime(t *testing.T) {
	now := time.Now()
	info := SysInfo{OS: "Ubuntu 22.04", Kernel: "5.15.0", BootAt: now.Add(-2 * time.Hour).UnixMilli()}

	if d := info.UptimeAt(now); d < 2*time.Hour-time.Minute || d > 2*time.Hour+time.Minute {
		t.Fatalf("运行时长推算错误：%v", d)
	}

	// 指纹变化只应来自这三个字段，任何一个变了都要重新上报。
	same := info
	if same.Key() != info.Key() {
		t.Fatal("相同内容应产生相同指纹")
	}
	if (SysInfo{OS: "Debian", Kernel: info.Kernel, BootAt: info.BootAt}).Key() == info.Key() {
		t.Fatal("发行版变化应改变指纹")
	}
	if (SysInfo{OS: info.OS, Kernel: "6.1.0", BootAt: info.BootAt}).Key() == info.Key() {
		t.Fatal("内核变化应改变指纹")
	}
	if (SysInfo{OS: info.OS, Kernel: info.Kernel, BootAt: 1}).Key() == info.Key() {
		t.Fatal("重启（开机时刻变化）应改变指纹")
	}

	// 开机时刻未知时不能算出离谱的运行时长。
	if d := (SysInfo{}).UptimeAt(now); d != 0 {
		t.Fatalf("开机时刻未知应返回 0，实际 %v", d)
	}
}

// 缓存应复用已读到的信息，而不是每次都重新读系统文件。
func TestSysCacheReuse(t *testing.T) {
	c := &sysCache{}
	now := time.Now()
	first := c.get(now)
	second := c.get(now.Add(time.Second))
	if first != second {
		t.Fatalf("1 秒内不应重新读取：%+v -> %+v", first, second)
	}
	// 超过 TTL 后会重新读取；内容本身在测试机上未必变化，只要求不 panic 且结构完整。
	if got := c.get(now.Add(sysInfoTTL + time.Second)); got.Key() == "" {
		t.Fatal("刷新后指纹不应为空")
	}
}

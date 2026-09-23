package collect

import (
	"strings"
	"testing"
)

// 取自真实 Linux 主机的 /proc/stat 片段（4 核）。
const procStatSample = `cpu  1234567 890 456789 98765432 12345 0 6789 0 0 0
cpu0 308641 222 114197 24691358 3086 0 1697 0 0 0
cpu1 308642 222 114197 24691358 3086 0 1697 0 0 0
intr 123456789
ctxt 987654321
`

const procMemInfoSample = `MemTotal:       16384000 kB
MemFree:         1000000 kB
MemAvailable:    6553600 kB
Buffers:          200000 kB
Cached:          4000000 kB
SwapTotal:             0 kB
SwapFree:              0 kB
`

func TestParseCPUTimes(t *testing.T) {
	idle, total, err := parseCPUTimes(strings.NewReader(procStatSample))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}

	// user+nice+system+idle+iowait+irq+softirq+steal，guest/guest_nice 不计入
	wantTotal := uint64(1234567 + 890 + 456789 + 98765432 + 12345 + 0 + 6789 + 0)
	if total != wantTotal {
		t.Errorf("total = %d, 期望 %d", total, wantTotal)
	}
	wantIdle := uint64(98765432 + 12345)
	if idle != wantIdle {
		t.Errorf("idle = %d, 期望 %d", idle, wantIdle)
	}
}

func TestParseCPUTimesInvalid(t *testing.T) {
	if _, _, err := parseCPUTimes(strings.NewReader("nothing here\n")); err == nil {
		t.Fatal("非法输入应返回错误")
	}
}

func TestParseMemInfo(t *testing.T) {
	total, avail, err := parseMemInfo(strings.NewReader(procMemInfoSample))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if total != 16384000 || avail != 6553600 {
		t.Fatalf("total=%d avail=%d", total, avail)
	}
	// 16000 MB 中有 6400 MB 可用 -> 占用 60%
	got := usagePercent(total, avail)
	if got < 59.9 || got > 60.1 {
		t.Fatalf("占用率 = %v, 期望 60", got)
	}
}

func TestParseMemInfoFallback(t *testing.T) {
	// 老内核没有 MemAvailable，应退化为 MemFree + Buffers + Cached
	const old = `MemTotal:       1000000 kB
MemFree:         100000 kB
Buffers:          50000 kB
Cached:          250000 kB
`
	total, avail, err := parseMemInfo(strings.NewReader(old))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if total != 1000000 || avail != 400000 {
		t.Fatalf("total=%d avail=%d, 期望 1000000 / 400000", total, avail)
	}
}

func TestCPUPercent(t *testing.T) {
	cases := []struct {
		dIdle, dTotal, want float64
	}{
		{100, 400, 75}, // 四分之一空闲
		{400, 400, 0},  // 全空闲
		{0, 400, 100},  // 全忙
		{50, 0, 0},     // 无增量
		{0, 0, 0},      // 除零保护
		{48, 400, 88},  // 常见场景
	}
	for _, c := range cases {
		if got := cpuPercent(c.dIdle, c.dTotal); got != c.want {
			t.Errorf("cpuPercent(%v,%v) = %v, 期望 %v", c.dIdle, c.dTotal, got, c.want)
		}
	}
}

func TestParseKBSuffix(t *testing.T) {
	v, err := parseKB("  16384000 kB")
	if err != nil || v != 16384000 {
		t.Fatalf("parseKB = %d, %v", v, err)
	}
}

func TestClampAndRound(t *testing.T) {
	if clamp(-5) != 0 || clamp(120) != 100 || clamp(50) != 50 {
		t.Fatal("clamp 行为不符合预期")
	}
	if round1(12.34) != 12.3 || round1(12.36) != 12.4 {
		t.Fatal("round1 行为不符合预期")
	}
}

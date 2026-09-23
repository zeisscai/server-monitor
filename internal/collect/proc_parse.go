package collect

import (
	"bufio"
	"errors"
	"io"
	"strconv"
	"strings"
)

// 本文件是与平台无关的 /proc 解析逻辑，单独抽出来便于在任意平台上做单元测试。

// parseCPUTimes 解析 /proc/stat 第一行，返回 (idle + iowait, total) 的 jiffies 计数。
//
// 字段顺序：user nice system idle iowait irq softirq steal guest guest_nice
// guest / guest_nice 已包含在 user / nice 中，因此不重复累加。
func parseCPUTimes(r io.Reader) (idle, total uint64, err error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)[1:]
		for i, fv := range fields {
			if i > 7 {
				break
			}
			v, perr := strconv.ParseUint(fv, 10, 64)
			if perr != nil {
				return 0, 0, perr
			}
			total += v
			if i == 3 || i == 4 {
				idle += v
			}
		}
		if total == 0 {
			return 0, 0, errors.New("无法解析 /proc/stat")
		}
		return idle, total, nil
	}
	return 0, 0, errors.New("无法解析 /proc/stat")
}

// parseMemInfo 解析 /proc/meminfo，返回 (MemTotal, MemAvailable)，单位为 kB。
// 老内核缺少 MemAvailable 时，退化为 MemFree + Buffers + Cached。
func parseMemInfo(r io.Reader) (total, available uint64, err error) {
	var free, buffers, cached uint64
	var hasAvailable bool

	sc := bufio.NewScanner(r)
	for sc.Scan() {
		key, val, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		n, perr := parseKB(val)
		if perr != nil {
			continue
		}
		switch key {
		case "MemTotal":
			total = n
		case "MemAvailable":
			available, hasAvailable = n, true
		case "MemFree":
			free = n
		case "Buffers":
			buffers = n
		case "Cached":
			cached = n
		}
	}
	if total == 0 {
		return 0, 0, errors.New("无法解析 /proc/meminfo")
	}
	if !hasAvailable {
		available = free + buffers + cached
	}
	if available > total {
		available = total
	}
	return total, available, nil
}

func parseKB(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, "kB")
	s = strings.TrimSpace(s)
	return strconv.ParseUint(s, 10, 64)
}

// usagePercent 计算占用率百分比。
func usagePercent(total, available uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(total-available) / float64(total) * 100
}

// cpuPercent 依据两次采样的累计值计算 CPU 占用率。
func cpuPercent(dIdle, dTotal float64) float64 {
	if dTotal <= 0 {
		return 0
	}
	return (dTotal - dIdle) / dTotal * 100
}

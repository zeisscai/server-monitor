package collect

import (
	"bufio"
	"io"
	"strconv"
	"strings"
)

// 本文件是与平台无关的纯解析逻辑，便于用单元测试覆盖各种真实输出格式。
// 平台相关的读取动作在 sysinfo_linux.go / sysinfo_darwin.go / sysinfo_other.go。

// parseOSRelease 从 os-release 内容中取出发行版本名称。
//
// 优先 PRETTY_NAME（"Ubuntu 22.04.3 LTS" 这种最适合展示的形式），
// 缺失时退回 "NAME VERSION_ID"。值两侧可能带单引号或双引号，需要去掉。
func parseOSRelease(r io.Reader) string {
	kv := map[string]string{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), 64*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		kv[strings.TrimSpace(k)] = unquote(strings.TrimSpace(v))
	}
	if v := kv["PRETTY_NAME"]; v != "" {
		return v
	}
	name := kv["NAME"]
	if name == "" {
		return ""
	}
	if v := kv["VERSION_ID"]; v != "" {
		return name + " " + v
	}
	return name
}

// unquote 去掉值两侧成对的引号。
func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// parseBtime 从 /proc/stat 中解析 btime 行（开机时刻，Unix 秒）。
//
// 该行并非所有内核都提供（部分容器、极老内核没有），缺失时返回 0。
func parseBtime(r io.Reader) int64 {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	for sc.Scan() {
		fields := strings.Fields(strings.TrimSpace(sc.Text()))
		if len(fields) < 2 || fields[0] != "btime" {
			continue
		}
		if v, err := strconv.ParseInt(fields[1], 10, 64); err == nil && v > 0 {
			return v
		}
		return 0
	}
	return 0
}

// parseUptime 从 /proc/uptime 解析开机时长（秒，浮点）。
func parseUptime(r io.Reader) float64 {
	b, err := io.ReadAll(io.LimitReader(r, 256))
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || v <= 0 {
		return 0
	}
	return v
}

// parseSysctlBoottime 解析 macOS 的 kern.boottime 输出。
//
// 形如：{ sec = 1699999999, usec = 123456 } Thu Nov  9 10:00:00 2023
func parseSysctlBoottime(s string) int64 {
	i := strings.Index(s, "sec =")
	if i < 0 {
		return 0
	}
	rest := s[i+len("sec ="):]
	end := strings.IndexAny(rest, ",}")
	if end >= 0 {
		rest = rest[:end]
	}
	v, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
	if err != nil || v <= 0 {
		return 0
	}
	return v
}

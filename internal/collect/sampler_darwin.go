//go:build darwin

// 说明：生产目标是 Linux 服务器，本文件仅为开发机（macOS）上的功能验证兜底实现。
// 它通过系统命令估算指标，精度与开销都不如 /proc 方案。

package collect

import (
	"errors"
	"os/exec"
	"strconv"
	"strings"
)

// Sampler 通过系统命令读取系统指标。
type Sampler struct{}

// New 创建采集器。
func New() *Sampler { return &Sampler{} }

// Read 返回本次采样结果。
func (s *Sampler) Read() (Sample, error) {
	cpu, err := readCPUUsage()
	if err != nil {
		return Sample{}, err
	}
	mem, err := readMemUsage()
	if err != nil {
		return Sample{}, err
	}
	return Sample{CPU: round1(clamp(cpu)), Mem: round1(clamp(mem))}, nil
}

func readCPUUsage() (float64, error) {
	if v, err := cpuFromTop(); err == nil {
		return v, nil
	}
	// 受限环境（如沙箱）无法执行 top 时，用 1 分钟负载 / 核心数 粗略估算。
	return cpuFromLoadAvg()
}

func cpuFromTop() (float64, error) {
	out, err := exec.Command("top", "-l", "2", "-s", "1", "-n", "0").Output()
	if err != nil {
		return 0, err
	}
	lines := strings.Split(string(out), "\n")
	idle := -1.0
	for _, ln := range lines {
		if !strings.HasPrefix(strings.TrimSpace(ln), "CPU usage:") {
			continue
		}
		for _, part := range strings.Split(ln, ",") {
			part = strings.TrimSpace(part)
			if strings.HasSuffix(part, "idle") {
				v := strings.TrimSpace(strings.TrimSuffix(part, "idle"))
				v = strings.TrimSuffix(v, "%")
				if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
					idle = f
				}
			}
		}
	}
	if idle < 0 {
		return 0, errors.New("无法解析 top 输出")
	}
	return 100 - idle, nil
}

func cpuFromLoadAvg() (float64, error) {
	out, err := exec.Command("sysctl", "-n", "vm.loadavg").Output()
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(strings.NewReplacer("{", " ", "}", " ", ",", " ").Replace(string(out)))
	if len(fields) == 0 {
		return 0, errors.New("无法读取负载")
	}
	load, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, err
	}
	ncpuOut, err := exec.Command("sysctl", "-n", "hw.ncpu").Output()
	if err != nil {
		return 0, err
	}
	ncpu, err := strconv.ParseFloat(strings.TrimSpace(string(ncpuOut)), 64)
	if err != nil || ncpu <= 0 {
		ncpu = 1
	}
	return load / ncpu * 100, nil
}

func readMemUsage() (float64, error) {
	totalOut, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return 0, err
	}
	total, err := strconv.ParseFloat(strings.TrimSpace(string(totalOut)), 64)
	if err != nil || total <= 0 {
		return 0, errors.New("无法读取物理内存大小")
	}

	vmOut, err := exec.Command("vm_stat").Output()
	if err != nil {
		return 0, err
	}
	pageSize := 4096.0
	usedPages := 0.0
	for _, ln := range strings.Split(string(vmOut), "\n") {
		if strings.Contains(ln, "page size of") {
			fields := strings.Fields(ln)
			for i, f := range fields {
				if f == "of" && i+1 < len(fields) {
					if v, err := strconv.ParseFloat(fields[i+1], 64); err == nil {
						pageSize = v
					}
				}
			}
			continue
		}
		key, val, ok := strings.Cut(ln, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		n, err := strconv.ParseFloat(strings.TrimSpace(strings.Trim(val, ". ")), 64)
		if err != nil {
			continue
		}
		switch key {
		case "Pages active", "Pages wired down", "Pages occupied by compressor":
			usedPages += n
		}
	}
	used := usedPages * pageSize
	if used > total {
		used = total
	}
	return used / total * 100, nil
}

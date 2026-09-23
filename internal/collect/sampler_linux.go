//go:build linux

package collect

import (
	"os"
	"time"
)

// Sampler 通过 /proc 读取系统指标。
type Sampler struct {
	prevIdle  uint64
	prevTotal uint64
	primed    bool
}

// New 创建采集器。
func New() *Sampler { return &Sampler{} }

// Read 返回本次采样结果。
// 首次调用会先取一次基线并短暂等待，以便直接得到有意义的 CPU 占用率。
func (s *Sampler) Read() (Sample, error) {
	idle, total, err := readCPUTimes()
	if err != nil {
		return Sample{}, err
	}
	if !s.primed {
		s.prevIdle, s.prevTotal = idle, total
		s.primed = true
		time.Sleep(200 * time.Millisecond)
		idle, total, err = readCPUTimes()
		if err != nil {
			return Sample{}, err
		}
	}

	dIdle := float64(idle - s.prevIdle)
	dTotal := float64(total - s.prevTotal)
	s.prevIdle, s.prevTotal = idle, total

	totalKB, availKB, err := readMemInfo()
	if err != nil {
		return Sample{}, err
	}
	return Sample{
		CPU: round1(clamp(cpuPercent(dIdle, dTotal))),
		Mem: round1(clamp(usagePercent(totalKB, availKB))),
	}, nil
}

func readCPUTimes() (uint64, uint64, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	return parseCPUTimes(f)
}

func readMemInfo() (uint64, uint64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	return parseMemInfo(f)
}

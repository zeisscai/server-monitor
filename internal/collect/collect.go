// Package collect 采集本机 CPU 占用率与内存占用率。
//
// 采集器是有状态的：CPU 占用率依赖两次采样之间的时间差（jiffies delta），
// 因此每个采集循环应持有一个独立的 Sampler 实例。
//
// 生产目标平台为 Linux（读取 /proc）；同时提供 darwin 兜底实现，
// 便于在开发机上验证整体流程。
package collect

// Sample 是一次采集结果，取值均为百分比（0 ~ 100）。
type Sample struct {
	CPU float64
	Mem float64
}

func clamp(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

func round1(v float64) float64 {
	return float64(int64(v*10+0.5)) / 10
}

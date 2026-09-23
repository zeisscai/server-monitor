//go:build !linux && !darwin

package collect

import "errors"

// Sampler 在不支持的平台上始终返回错误。
type Sampler struct{}

// New 创建采集器。
func New() *Sampler { return &Sampler{} }

// Read 当前平台不支持采集。
func (s *Sampler) Read() (Sample, error) {
	return Sample{}, errors.New("当前平台不支持采集，请在 Linux 上运行")
}

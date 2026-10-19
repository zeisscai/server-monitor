//go:build linux || darwin

package main

import "syscall"

// maxOpenFiles 是期望的文件句柄上限。
//
// 每台上报端会保持一条长连接，千台规模即需要 1000+ 个 fd；而不少发行版默认软限制
// 只有 1024，systemd 单元未显式配置时同样是 1024。fd 一旦被占满，表现得像是
// "网络抖了一下"（accept 失败 / 连接被重置），很难从日志里看出来，因此启动时主动抬高。
const maxOpenFiles = 65535

// raiseFileLimit 把进程的文件句柄软限制提升到硬限制（最多 maxOpenFiles）。
func raiseFileLimit() (uint64, error) {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		return 0, err
	}
	if lim.Cur >= maxOpenFiles {
		return lim.Cur, nil
	}
	target := lim.Max
	if target > maxOpenFiles {
		target = maxOpenFiles
	}
	lim.Cur = target
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		return lim.Cur, err
	}
	return target, nil
}

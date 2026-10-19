//go:build !linux && !darwin

package main

// raiseFileLimit 在非 Unix 平台上不做任何事（本程序只在 Linux / macOS 上部署）。
func raiseFileLimit() (uint64, error) { return 0, nil }

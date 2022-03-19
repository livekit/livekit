//go:build mage && !windows
// +build mage,!windows

package main

import (
	"syscall"
)

func setULimit() error {
	// raise ulimit on unix
	var rLimit syscall.Rlimit
	err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rLimit)
	if err != nil {
		return err
	}
	rLimit.Max = 10000
	rLimit.Cur = 10000
	return syscall.Setrlimit(syscall.RLIMIT_NOFILE, &rLimit)
}

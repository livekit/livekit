//go:build linux
// +build linux

package prometheus

import (
	linuxproc "github.com/c9s/goprocinfo/linux"
)

func getSystemStats() (numCPUs uint32, avg1Min, avg5Min, avg15Min float32, err error) {
	cpuInfo, err := linuxproc.ReadCPUInfo("/proc/cpuinfo")
	if err != nil {
		return
	}

	loadAvg, err := linuxproc.ReadLoadAvg("/proc/loadavg")
	if err != nil {
		return
	}

	numCPUs = uint32(cpuInfo.NumCPU())
	avg1Min = float32(loadAvg.Last1Min)
	avg5Min = float32(loadAvg.Last5Min)
	avg15Min = float32(loadAvg.Last15Min)
	return
}

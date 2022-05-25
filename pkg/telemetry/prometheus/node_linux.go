//go:build linux
// +build linux

package prometheus

import (
	"sync"

	"github.com/mackerelio/go-osstat/cpu"
)

var cpuStatsLock sync.RWMutex
var lastCPUTotal, lastCPUIdle uint64

func getCPUStats() (cpuLoad float32, numCPUs uint32, err error) {
	cpuInfo, err := cpu.Get()
	if err != nil {
		return
	}

	cpuStatsLock.Lock()
	if lastCPUTotal > 0 && lastCPUTotal < cpuInfo.Total {
		cpuLoad = 1 - float32(cpuInfo.Idle-lastCPUIdle)/float32(cpuInfo.Total-lastCPUTotal)
	}

	lastCPUTotal = cpuInfo.Total
	lastCPUIdle = cpuInfo.Idle // + cpu.Iowait
	cpuStatsLock.Unlock()

	numCPUs = uint32(cpuInfo.CPUCount)

	return
}

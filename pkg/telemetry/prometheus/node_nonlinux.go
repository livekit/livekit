//go:build !linux
// +build !linux

package prometheus

import (
	"github.com/mackerelio/go-osstat/cpu"
)

var lastCPUTotal, lastCPUIdle uint64

func getCPUStats() (cpuLoad float32, numCPUs uint32, err error) {
	cpuInfo, err := cpu.Get()
	if err != nil {
		return
	}

	if lastCPUTotal > 0 && lastCPUTotal < cpuInfo.Total {
		cpuLoad = 1 - float32(cpuInfo.Idle-lastCPUIdle)/float32(cpuInfo.Total-lastCPUTotal)
	}

	lastCPUTotal = cpuInfo.Total
	lastCPUIdle = cpuInfo.Idle

	return
}

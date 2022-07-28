//go:build linux
// +build linux

package prometheus

import (
	"fmt"
	"sync"

	"github.com/florianl/go-tc"
	"github.com/mackerelio/go-osstat/cpu"
)

var (
	cpuStatsLock              sync.RWMutex
	lastCPUTotal, lastCPUIdle uint64
)

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

func getTCStats() (packets, drops uint32, err error) {
	rtnl, err := tc.Open(&tc.Config{})
	if err != nil {
		err = fmt.Errorf("could not open rtnetlink socket: %v", err)
		return
	}
	defer rtnl.Close()

	qdiscs, err := rtnl.Qdisc().Get()
	if err != nil {
		err = fmt.Errorf("could not get qdiscs: %v", err)
		return
	}

	for _, qdisc := range qdiscs {
		packets = packets + qdisc.Stats.Packets
		drops = drops + qdisc.Stats.Drops
	}

	return
}

// +build linux

package stats

import (
	linuxproc "github.com/c9s/goprocinfo/linux"
	livekit "github.com/livekit/protocol/proto"
)

func updateCurrentNodeSystemStats(nodeStats *livekit.NodeStats) error {
	cpuInfo, err := linuxproc.ReadCPUInfo("/proc/cpuinfo")
	if err != nil {
		return err
	}

	loadAvg, err := linuxproc.ReadLoadAvg("/proc/loadavg")
	if err != nil {
		return err
	}

	nodeStats.NumCpus = uint32(cpuInfo.NumCPU())
	nodeStats.LoadAvgLast1Min = float32(loadAvg.Last1Min)
	nodeStats.LoadAvgLast5Min = float32(loadAvg.Last5Min)
	nodeStats.LoadAvgLast15Min = float32(loadAvg.Last15Min)

	return nil
}

//go:build linux
// +build linux

package prometheus

import (
	linuxproc "github.com/c9s/goprocinfo/linux"
	"github.com/livekit/protocol/livekit"
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

	// logger.Infow("cpuinfo", cpuInfo)
	// logger.Infow("loadAvg", loadAvg)
	return nil
}

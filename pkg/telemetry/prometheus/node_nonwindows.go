//go:build !windows

/*
 * Copyright 2023 LiveKit, Inc
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package prometheus

import (
	"runtime"
	"sync"

	"github.com/mackerelio/go-osstat/cpu"
	"github.com/mackerelio/go-osstat/loadavg"
)

var (
	cpuStatsLock              sync.RWMutex
	lastCPUTotal, lastCPUIdle uint64
)

func getLoadAvg() (*loadavg.Stats, error) {
	return loadavg.Get()
}

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
	lastCPUIdle = cpuInfo.Idle
	cpuStatsLock.Unlock()

	numCPUs = uint32(runtime.NumCPU())

	return
}

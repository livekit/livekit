// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package pacer

import (
	"sync"
	"time"

	"go.uber.org/atomic"

	"github.com/livekit/livekit-server/pkg/sfu/ccutils"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils/mono"
)

type ProbeObserver struct {
	logger logger.Logger

	listener PacerProbeObserverListener

	isInProbe atomic.Bool

	lock sync.Mutex
	pci  ccutils.ProbeClusterInfo
}

func NewProbeObserver(logger logger.Logger) *ProbeObserver {
	return &ProbeObserver{
		logger: logger,
	}
}

func (po *ProbeObserver) SetPacerProbeObserverListener(listener PacerProbeObserverListener) {
	po.listener = listener
}

func (po *ProbeObserver) StartProbeCluster(pci ccutils.ProbeClusterInfo) {
	if po.isInProbe.Load() {
		po.logger.Warnw(
			"ignoring start of a new probe cluster when already active", nil,
			"probeClusterInfo", pci,
		)
		return
	}

	po.lock.Lock()
	defer po.lock.Unlock()

	po.pci = pci
	po.pci.Result = ccutils.ProbeClusterResult{
		StartTime: mono.UnixNano(),
	}

	po.isInProbe.Store(true)
}

func (po *ProbeObserver) EndProbeCluster(probeClusterId ccutils.ProbeClusterId) ccutils.ProbeClusterInfo {
	if !po.isInProbe.Load() {
		// probe not active
		if probeClusterId != ccutils.ProbeClusterIdInvalid {
			po.logger.Debugw(
				"ignoring end of a probe cluster when not active",
				"probeClusterId", probeClusterId,
			)
		}
		return ccutils.ProbeClusterInfoInvalid
	}

	po.lock.Lock()
	defer po.lock.Unlock()

	if po.pci.Id != probeClusterId {
		// probe cluster id not active
		po.logger.Warnw(
			"ignoring end of a probe cluster of a non-active one", nil,
			"probeClusterId", probeClusterId,
			"active", po.pci.Id,
		)
		return ccutils.ProbeClusterInfoInvalid
	}

	if po.pci.Result.EndTime == 0 {
		po.pci.Result.EndTime = mono.UnixNano()
	}

	po.isInProbe.Store(false)

	return po.pci
}

func (po *ProbeObserver) RecordPacket(size int, isRTX bool, probeClusterId ccutils.ProbeClusterId, isProbe bool) {
	if !po.isInProbe.Load() {
		return
	}

	po.lock.Lock()
	if probeClusterId != po.pci.Id || po.pci.Result.EndTime != 0 {
		po.lock.Unlock()
		return
	}

	if isProbe {
		po.pci.Result.PacketsProbe++
		po.pci.Result.BytesProbe += size
	} else {
		if isRTX {
			po.pci.Result.PacketsNonProbeRTX++
			po.pci.Result.BytesNonProbeRTX += size
		} else {
			po.pci.Result.PacketsNonProbePrimary++
			po.pci.Result.BytesNonProbePrimary += size
		}
	}

	notify := false
	var clusterId ccutils.ProbeClusterId
	if po.pci.Result.EndTime == 0 && ((po.pci.Result.Bytes() >= po.pci.Goal.DesiredBytes) && time.Duration(mono.UnixNano()-po.pci.Result.StartTime) >= po.pci.Goal.Duration) {
		po.pci.Result.EndTime = mono.UnixNano()
		po.pci.Result.IsCompleted = true

		notify = true
		clusterId = po.pci.Id
	}
	po.lock.Unlock()

	if notify && po.listener != nil {
		po.listener.OnPacerProbeObserverClusterComplete(clusterId)
	}
}

// ------------------------------------------------

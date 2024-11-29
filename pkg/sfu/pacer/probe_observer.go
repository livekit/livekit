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
	"sync/atomic"

	"github.com/livekit/livekit-server/pkg/sfu/ccutils"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils/mono"
)

type ProbeObserver struct {
	logger logger.Logger

	listener PacerProbeObserverListener

	isInProbe atomic.Bool

	lock                     sync.Mutex
	clusterStartTime         int64
	activeProbeClusterId     ccutils.ProbeClusterId
	desiredProbeClusterBytes int
	bytesNonProbePrimary     int
	bytesNonProbeRTX         int
	bytesProbe               int
	isActiveClusterDone      bool
}

func NewProbeObserver(logger logger.Logger) *ProbeObserver {
	return &ProbeObserver{
		logger: logger,
	}
}

func (po *ProbeObserver) SetPacerProbeObserverListener(listener PacerProbeObserverListener) {
	po.listener = listener
}

func (po *ProbeObserver) StartProbeCluster(probeClusterId ccutils.ProbeClusterId, desiredBytes int) {
	if po.isInProbe.Load() {
		po.logger.Warnw(
			"ignoring start of a new probe cluster when already active", nil,
			"probeClusterId", probeClusterId,
			"desiredBytes", desiredBytes,
		)
		return
	}

	po.lock.Lock()
	defer po.lock.Unlock()

	po.clusterStartTime = mono.UnixNano()
	po.activeProbeClusterId = probeClusterId
	po.desiredProbeClusterBytes = desiredBytes
	po.bytesNonProbePrimary = 0
	po.bytesNonProbeRTX = 0
	po.bytesProbe = 0
	po.isActiveClusterDone = false

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

	if po.activeProbeClusterId != probeClusterId {
		// probe cluster id not active
		po.logger.Warnw(
			"ignoring end of a probe cluster of a non-active one", nil,
			"probeClusterId", probeClusterId,
			"active", po.activeProbeClusterId,
		)
		return ccutils.ProbeClusterInfoInvalid
	}

	clusterInfo := ccutils.ProbeClusterInfo{
		ProbeClusterId:       po.activeProbeClusterId,
		DesiredBytes:         po.desiredProbeClusterBytes,
		StartTime:            po.clusterStartTime,
		EndTime:              mono.UnixNano(),
		BytesProbe:           po.bytesProbe,
		BytesNonProbePrimary: po.bytesNonProbePrimary,
		BytesNonProbeRTX:     po.bytesNonProbeRTX,
	}

	po.activeProbeClusterId = ccutils.ProbeClusterIdInvalid
	po.isInProbe.Store(false)

	return clusterInfo
}

func (po *ProbeObserver) RecordPacket(size int, isRTX bool, probeClusterId ccutils.ProbeClusterId, isProbe bool) {
	if !po.isInProbe.Load() {
		return
	}

	po.lock.Lock()
	if probeClusterId != po.activeProbeClusterId || po.isActiveClusterDone {
		po.lock.Unlock()
		return
	}

	if isProbe {
		po.bytesProbe += size
	} else {
		if isRTX {
			po.bytesNonProbeRTX += size
		} else {
			po.bytesNonProbePrimary += size
		}
	}

	notify := false
	var clusterId ccutils.ProbeClusterId
	if !po.isActiveClusterDone && po.bytesProbe+po.bytesNonProbePrimary+po.bytesNonProbeRTX >= po.desiredProbeClusterBytes {
		po.isActiveClusterDone = true

		notify = true
		clusterId = po.activeProbeClusterId
	}
	po.lock.Unlock()

	if notify && po.listener != nil {
		po.listener.OnPacerProbeObserverClusterComplete(clusterId)
	}
}

// ------------------------------------------------

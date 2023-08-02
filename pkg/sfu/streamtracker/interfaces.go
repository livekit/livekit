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

package streamtracker

import (
	"fmt"
	"time"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
)

// ------------------------------------------------------------

type StreamStatusChange int32

func (s StreamStatusChange) String() string {
	switch s {
	case StreamStatusChangeNone:
		return "none"
	case StreamStatusChangeStopped:
		return "stopped"
	case StreamStatusChangeActive:
		return "active"
	default:
		return fmt.Sprintf("unknown: %d", int(s))
	}
}

const (
	StreamStatusChangeNone StreamStatusChange = iota
	StreamStatusChangeStopped
	StreamStatusChangeActive
)

// ------------------------------------------------------------

type StreamTrackerImpl interface {
	Start()
	Stop()
	Reset()

	GetCheckInterval() time.Duration

	Observe(hasMarker bool, ts uint32) StreamStatusChange
	CheckStatus() StreamStatusChange
}

type StreamTrackerWorker interface {
	Start()
	Stop()
	Reset()
	OnStatusChanged(f func(status StreamStatus))
	OnBitrateAvailable(f func())
	Status() StreamStatus
	BitrateTemporalCumulative() []int64
	SetPaused(paused bool)
	Observe(temporalLayer int32, pktSize int, payloadSize int, hasMarker bool, ts uint32, dd *buffer.ExtDependencyDescriptor)
}

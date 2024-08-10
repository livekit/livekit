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
	"sync"
	"time"

	"go.uber.org/atomic"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	dd "github.com/livekit/livekit-server/pkg/sfu/rtpextension/dependencydescriptor"
)

type StreamTrackerDependencyDescriptor struct {
	lock             sync.RWMutex
	paused           bool
	generation       atomic.Uint32
	params           StreamTrackerParams
	maxSpatialLayer  int32
	maxTemporalLayer int32

	onStatusChanged    [buffer.DefaultMaxLayerSpatial + 1]func(status StreamStatus)
	onBitrateAvailable [buffer.DefaultMaxLayerSpatial + 1]func()

	lastBitrateReport time.Time
	bytesForBitrate   [buffer.DefaultMaxLayerSpatial + 1][buffer.DefaultMaxLayerTemporal + 1]int64
	bitrate           [buffer.DefaultMaxLayerSpatial + 1][buffer.DefaultMaxLayerTemporal + 1]int64

	isStopped bool
}

func NewStreamTrackerDependencyDescriptor(params StreamTrackerParams) *StreamTrackerDependencyDescriptor {
	return &StreamTrackerDependencyDescriptor{
		params:           params,
		maxSpatialLayer:  buffer.InvalidLayerSpatial,
		maxTemporalLayer: buffer.InvalidLayerTemporal,
	}
}
func (s *StreamTrackerDependencyDescriptor) Start() {
}

func (s *StreamTrackerDependencyDescriptor) Stop() {
	s.lock.Lock()
	defer s.lock.Unlock()

	if s.isStopped {
		return
	}
	s.isStopped = true

	// bump generation to trigger exit of worker
	s.generation.Inc()
}

func (s *StreamTrackerDependencyDescriptor) OnStatusChanged(layer int32, f func(status StreamStatus)) {
	s.lock.Lock()
	s.onStatusChanged[layer] = f
	s.lock.Unlock()
}

func (s *StreamTrackerDependencyDescriptor) OnBitrateAvailable(layer int32, f func()) {
	s.lock.Lock()
	s.onBitrateAvailable[layer] = f
	s.lock.Unlock()
}

func (s *StreamTrackerDependencyDescriptor) Status(layer int32) StreamStatus {
	s.lock.RLock()
	defer s.lock.RUnlock()

	if layer > s.maxSpatialLayer {
		return StreamStatusStopped
	}

	return StreamStatusActive
}

func (s *StreamTrackerDependencyDescriptor) BitrateTemporalCumulative(layer int32) []int64 {
	s.lock.RLock()
	defer s.lock.RUnlock()

	if layer > s.maxSpatialLayer {
		brs := make([]int64, len(s.bitrate[0]))
		return brs
	}

	return s.bitrate[layer][:]
}

func (s *StreamTrackerDependencyDescriptor) Reset() {
}

func (s *StreamTrackerDependencyDescriptor) resetLocked() {
	// bump generation to trigger exit of current worker
	s.generation.Inc()

	for i := 0; i < len(s.bytesForBitrate); i++ {
		for j := 0; j < len(s.bytesForBitrate[i]); j++ {
			s.bytesForBitrate[i][j] = 0
		}
	}
	for i := 0; i < len(s.bitrate); i++ {
		for j := 0; j < len(s.bitrate[i]); j++ {
			s.bitrate[i][j] = 0
		}
	}

	s.maxSpatialLayer = buffer.InvalidLayerSpatial
	s.maxTemporalLayer = buffer.InvalidLayerTemporal
}

func (s *StreamTrackerDependencyDescriptor) SetPaused(paused bool) {
	s.lock.Lock()
	if s.paused == paused {
		s.lock.Unlock()
		return
	}
	s.paused = paused

	var notifyFns []func(status StreamStatus)
	var notifyStatus StreamStatus
	if !paused {
		s.resetLocked()

		notifyStatus = StreamStatusStopped
		notifyFns = append(notifyFns, s.onStatusChanged[:]...)
	} else {
		s.lastBitrateReport = time.Now()
		go s.worker(s.generation.Inc())

	}
	s.lock.Unlock()

	for _, fn := range notifyFns {
		if fn != nil {
			fn(notifyStatus)
		}
	}
}

func (s *StreamTrackerDependencyDescriptor) Observe(temporalLayer int32, pktSize int, payloadSize int, hasMarker bool, ts uint32, ddVal *buffer.ExtDependencyDescriptor) {
	s.lock.Lock()

	if s.isStopped || s.paused || payloadSize == 0 || ddVal == nil {
		s.lock.Unlock()
		return
	}

	var notifyFns []func(status StreamStatus)
	var notifyStatus StreamStatus
	if mask := ddVal.Descriptor.ActiveDecodeTargetsBitmask; mask != nil && ddVal.ActiveDecodeTargetsUpdated {
		var maxSpatial, maxTemporal int32
		for _, dt := range ddVal.DecodeTargets {
			if *mask&(1<<dt.Target) != uint32(dd.DecodeTargetNotPresent) {
				if maxSpatial < dt.Layer.Spatial {
					maxSpatial = dt.Layer.Spatial
				}
				if maxTemporal < dt.Layer.Temporal {
					maxTemporal = dt.Layer.Temporal
				}
			}
		}
		if maxSpatial > buffer.DefaultMaxLayerSpatial {
			maxSpatial = buffer.DefaultMaxLayerSpatial
			s.params.Logger.Warnw("max spatial layer exceeded", nil, "maxSpatial", maxSpatial)
		}
		if maxTemporal > buffer.DefaultMaxLayerTemporal {
			maxTemporal = buffer.DefaultMaxLayerTemporal
			s.params.Logger.Warnw("max temporal layer exceeded", nil, "maxTemporal", maxTemporal)
		}

		s.params.Logger.Debugw("max layer changed", "maxSpatial", maxSpatial, "maxTemporal", maxTemporal)
		oldMaxSpatial := s.maxSpatialLayer
		s.maxSpatialLayer, s.maxTemporalLayer = maxSpatial, maxTemporal
		if oldMaxSpatial == -1 {
			s.lastBitrateReport = time.Now()
			go s.worker(s.generation.Inc())
		}

		if oldMaxSpatial > s.maxSpatialLayer {
			notifyStatus = StreamStatusStopped
			for i := s.maxSpatialLayer + 1; i <= oldMaxSpatial; i++ {
				notifyFns = append(notifyFns, s.onStatusChanged[i])
			}
		} else if oldMaxSpatial < s.maxSpatialLayer {
			notifyStatus = StreamStatusActive
			for i := oldMaxSpatial + 1; i <= s.maxSpatialLayer; i++ {
				notifyFns = append(notifyFns, s.onStatusChanged[i])
			}
		}
	}

	dtis := ddVal.Descriptor.FrameDependencies.DecodeTargetIndications

	for _, dt := range ddVal.DecodeTargets {
		if len(dtis) <= dt.Target {
			s.params.Logger.Errorw("len(dtis) less than target", nil, "target", dt.Target, "dtis", dtis)
			continue
		}
		// we are not dropping discardable frames now, so only ingore not present frames
		if dtis[dt.Target] == dd.DecodeTargetNotPresent {
			continue
		}

		s.bytesForBitrate[dt.Layer.Spatial][dt.Layer.Temporal] += int64(pktSize)
	}

	s.lock.Unlock()

	for _, fn := range notifyFns {
		if fn != nil {
			fn(notifyStatus)
		}
	}
}

func (s *StreamTrackerDependencyDescriptor) worker(generation uint32) {
	tickerBitrate := time.NewTicker(s.params.BitrateReportInterval)
	defer tickerBitrate.Stop()

	for {
		<-tickerBitrate.C
		if generation != s.generation.Load() {
			return
		}
		s.bitrateReport()
	}
}

func (s *StreamTrackerDependencyDescriptor) bitrateReport() {
	// run this even if paused to drain out bitrate if there are no packets coming in
	s.lock.Lock()
	now := time.Now()
	diff := now.Sub(s.lastBitrateReport)
	s.lastBitrateReport = now

	var availableChangedFns []func()
	for spatial := 0; spatial < len(s.bytesForBitrate); spatial++ {
		bytesForBitrate := s.bytesForBitrate[spatial][:]
		bitrateAvailabilityChanged := false
		bitrates := s.bitrate[spatial][:]
		for i := 0; i < len(bytesForBitrate); i++ {
			bitrate := int64(float64(bytesForBitrate[i]*8) / diff.Seconds())
			if (bitrates[i] == 0 && bitrate > 0) || (bitrates[i] > 0 && bitrate == 0) {
				bitrateAvailabilityChanged = true
			}
			bitrates[i] = bitrate
			bytesForBitrate[i] = 0
		}

		if bitrateAvailabilityChanged && s.onBitrateAvailable[spatial] != nil {
			availableChangedFns = append(availableChangedFns, s.onBitrateAvailable[spatial])
		}
	}
	s.lock.Unlock()

	for _, fn := range availableChangedFns {
		fn()
	}
}

func (s *StreamTrackerDependencyDescriptor) LayeredTracker(layer int32) *StreamTrackerDependencyDescriptorLayered {
	return &StreamTrackerDependencyDescriptorLayered{
		StreamTrackerDependencyDescriptor: s,
		layer:                             layer,
	}
}

// ----------------------------
// Layered wrapper for StreamTrackerWorker
type StreamTrackerDependencyDescriptorLayered struct {
	*StreamTrackerDependencyDescriptor
	layer int32
}

func (s *StreamTrackerDependencyDescriptorLayered) OnStatusChanged(f func(status StreamStatus)) {
	s.StreamTrackerDependencyDescriptor.OnStatusChanged(s.layer, f)
}

func (s *StreamTrackerDependencyDescriptorLayered) OnBitrateAvailable(f func()) {
	s.StreamTrackerDependencyDescriptor.OnBitrateAvailable(s.layer, f)
}

func (s *StreamTrackerDependencyDescriptorLayered) Status() StreamStatus {
	return s.StreamTrackerDependencyDescriptor.Status(s.layer)
}

func (s *StreamTrackerDependencyDescriptorLayered) BitrateTemporalCumulative() []int64 {
	return s.StreamTrackerDependencyDescriptor.BitrateTemporalCumulative(s.layer)
}

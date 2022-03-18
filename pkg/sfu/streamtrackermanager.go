package sfu

import (
	"sort"
	"sync"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

var (
	exemptLayers = []int32{0}
)

var (
	ConfigVideo = []StreamTrackerParams{
		{
			SamplesRequired: 1,
			CyclesRequired:  4,
			CycleDuration:   500 * time.Millisecond,
		},
		{
			SamplesRequired: 5,
			CyclesRequired:  60,
			CycleDuration:   500 * time.Millisecond,
		},
		{
			SamplesRequired: 5,
			CyclesRequired:  60,
			CycleDuration:   500 * time.Millisecond,
		},
	}

	// be very forgiving for screen share to account for cases like static screen where there could be only one packet per second
	ConfigScreenshare = []StreamTrackerParams{
		{
			SamplesRequired: 1,
			CyclesRequired:  1,
			CycleDuration:   2 * time.Second,
		},
		{
			SamplesRequired: 1,
			CyclesRequired:  1,
			CycleDuration:   2 * time.Second,
		},
		{
			SamplesRequired: 1,
			CyclesRequired:  1,
			CycleDuration:   2 * time.Second,
		},
	}
)

type StreamTrackerManager struct {
	logger logger.Logger
	source livekit.TrackSource

	lock sync.RWMutex

	trackers [DefaultMaxLayerSpatial + 1]*StreamTracker

	availableLayers  []int32
	maxExpectedLayer int32

	onAvailableLayersChanged func(availableLayers []int32)
}

func NewStreamTrackerManager(logger logger.Logger, source livekit.TrackSource) *StreamTrackerManager {
	return &StreamTrackerManager{
		logger:           logger,
		source:           source,
		maxExpectedLayer: DefaultMaxLayerSpatial,
	}
}

func (s *StreamTrackerManager) OnAvailableLayersChanged(f func(availableLayers []int32)) {
	s.onAvailableLayersChanged = f
}

func (s *StreamTrackerManager) AddTracker(layer int32) {
	var params StreamTrackerParams
	if s.source == livekit.TrackSource_SCREEN_SHARE {
		if int(layer) >= len(ConfigScreenshare) {
			return
		}

		params = ConfigScreenshare[layer]
	} else {
		if int(layer) >= len(ConfigVideo) {
			return
		}

		params = ConfigVideo[layer]
	}
	params.Logger = s.logger
	tracker := NewStreamTracker(params)
	tracker.OnStatusChanged(func(status StreamStatus) {
		if status == StreamStatusStopped {
			exempt := false
			for _, l := range exemptLayers {
				if layer == l {
					exempt = true
					break
				}
			}
			if !exempt {
				s.removeAvailableLayer(layer)
			} else {
				s.logger.Debugw("not removing exempt layer", "layer", layer)
			}
		} else {
			s.addAvailableLayer(layer)
		}
	})

	s.lock.Lock()
	s.trackers[layer] = tracker
	s.lock.Unlock()

	tracker.Start()
}

func (s *StreamTrackerManager) RemoveTracker(layer int32) {
	s.lock.Lock()
	tracker := s.trackers[layer]
	s.trackers[layer] = nil
	s.lock.Unlock()

	if tracker != nil {
		tracker.Stop()
	}
}

func (s *StreamTrackerManager) RemoveAllTrackers() {
	s.lock.Lock()
	trackers := s.trackers
	for layer := range s.trackers {
		s.trackers[layer] = nil
	}
	s.lock.Unlock()

	for _, tracker := range trackers {
		if tracker != nil {
			tracker.Stop()
		}
	}
}

func (s *StreamTrackerManager) GetTracker(layer int32) *StreamTracker {
	s.lock.RLock()
	defer s.lock.RUnlock()

	return s.trackers[layer]
}

func (s *StreamTrackerManager) SetPaused(paused bool) {
	s.lock.Lock()
	trackers := s.trackers
	s.lock.Unlock()

	for _, tracker := range trackers {
		if tracker != nil {
			tracker.SetPaused(paused)
		}
	}
}

func (s *StreamTrackerManager) SetMaxExpectedSpatialLayer(layer int32) {
	s.lock.Lock()
	if layer <= s.maxExpectedLayer {
		// some higher layer(s) expected to stop, nothing else to do
		s.maxExpectedLayer = layer
		s.lock.Unlock()
		return
	}

	//
	// Some higher layer is expected to start.
	// If the layer was not stopped (i.e. it will still be in available layers),
	// don't need to do anything. If not, reset the stream tracker so that
	// the layer is declared available on the first packet
	//
	// NOTE: There may be a race between checking if a layer is available and
	// resetting the tracker, i.e. the track may stop just after checking.
	// But, those conditions should be rare. In those cases, the restart will
	// take longer.
	//
	var trackersToReset []*StreamTracker
	for l := s.maxExpectedLayer + 1; l <= layer; l++ {
		if s.hasSpatialLayerLocked(l) {
			continue
		}

		if s.trackers[l] != nil {
			trackersToReset = append(trackersToReset, s.trackers[l])
		}
	}
	s.maxExpectedLayer = layer
	s.lock.Unlock()

	for _, tracker := range trackersToReset {
		tracker.Reset()
	}
}

func (s *StreamTrackerManager) IsReducedQuality() bool {
	s.lock.RLock()
	defer s.lock.RUnlock()

	return int32(len(s.availableLayers)) < (s.maxExpectedLayer + 1)
}

func (s *StreamTrackerManager) GetAvailableLayers() []int32 {
	s.lock.RLock()
	defer s.lock.RUnlock()

	return s.availableLayers
}

func (s *StreamTrackerManager) HasSpatialLayer(layer int32) bool {
	s.lock.RLock()
	defer s.lock.RUnlock()

	return s.hasSpatialLayerLocked(layer)
}

func (s *StreamTrackerManager) hasSpatialLayerLocked(layer int32) bool {
	for _, l := range s.availableLayers {
		if l == layer {
			return true
		}
	}

	return false
}

func (s *StreamTrackerManager) addAvailableLayer(layer int32) {
	s.lock.Lock()
	hasLayer := false
	for _, l := range s.availableLayers {
		if l == layer {
			hasLayer = true
			break
		}
	}
	if hasLayer {
		s.lock.Unlock()
		return
	}

	s.availableLayers = append(s.availableLayers, layer)
	sort.Slice(s.availableLayers, func(i, j int) bool { return s.availableLayers[i] < s.availableLayers[j] })
	layers := s.availableLayers
	s.lock.Unlock()

	s.logger.Debugw("available layers changed", "layers", layers)

	if s.onAvailableLayersChanged != nil {
		s.onAvailableLayersChanged(layers)
	}
}

func (s *StreamTrackerManager) removeAvailableLayer(layer int32) {
	s.lock.Lock()
	newLayers := make([]int32, 0, DefaultMaxLayerSpatial+1)
	for _, l := range s.availableLayers {
		if l != layer {
			newLayers = append(newLayers, l)
		}
	}
	sort.Slice(newLayers, func(i, j int) bool { return newLayers[i] < newLayers[j] })
	s.availableLayers = newLayers
	s.lock.Unlock()

	s.logger.Debugw("available layers changed", "layers", newLayers)

	// need to immediately switch off unavailable layers
	if s.onAvailableLayersChanged != nil {
		s.onAvailableLayersChanged(newLayers)
	}
}

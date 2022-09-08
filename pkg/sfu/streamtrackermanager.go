package sfu

import (
	"sort"
	"sync"
	"time"

	"github.com/go-logr/logr"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

var (
	ExemptedLayersScreenshare = []int32{0}
	ExemptedLayersVideo       = []int32{}
)

var (
	ConfigVideo = []StreamTrackerParams{
		{
			SamplesRequired:       1,
			CyclesRequired:        4,
			CycleDuration:         500 * time.Millisecond,
			BitrateReportInterval: 1 * time.Second,
		},
		{
			SamplesRequired:       5,
			CyclesRequired:        20,
			CycleDuration:         500 * time.Millisecond,
			BitrateReportInterval: 1 * time.Second,
		},
		{
			SamplesRequired:       5,
			CyclesRequired:        20,
			CycleDuration:         500 * time.Millisecond,
			BitrateReportInterval: 1 * time.Second,
		},
	}

	// be very forgiving for screen share to account for cases like static screen where there could be only one packet per second
	ConfigScreenshare = []StreamTrackerParams{
		{
			SamplesRequired:       1,
			CyclesRequired:        1,
			CycleDuration:         2 * time.Second,
			BitrateReportInterval: 4 * time.Second,
		},
		{
			SamplesRequired:       1,
			CyclesRequired:        1,
			CycleDuration:         2 * time.Second,
			BitrateReportInterval: 4 * time.Second,
		},
		{
			SamplesRequired:       1,
			CyclesRequired:        1,
			CycleDuration:         2 * time.Second,
			BitrateReportInterval: 4 * time.Second,
		},
	}
)

type StreamTrackerManager struct {
	logger            logger.Logger
	trackInfo         *livekit.TrackInfo
	maxPublishedLayer int32

	lock sync.RWMutex

	trackers [DefaultMaxLayerSpatial + 1]*StreamTracker

	availableLayers  []int32
	exemptedLayers   []int32
	maxExpectedLayer int32
	paused           bool

	onAvailableLayersChanged     func(availableLayers []int32, exemptedLayers []int32)
	onBitrateAvailabilityChanged func()
	onMaxLayerChanged            func(maxLayer int32)
}

func NewStreamTrackerManager(logger logger.Logger, trackInfo *livekit.TrackInfo) *StreamTrackerManager {
	s := &StreamTrackerManager{
		logger:            logger,
		trackInfo:         trackInfo,
		maxPublishedLayer: 0,
	}

	for _, layer := range s.trackInfo.Layers {
		spatialLayer := buffer.VideoQualityToSpatialLayer(layer.Quality, trackInfo)
		if spatialLayer > s.maxPublishedLayer {
			s.maxPublishedLayer = spatialLayer
		}
	}
	s.maxExpectedLayer = s.maxPublishedLayer

	return s
}

func (s *StreamTrackerManager) OnAvailableLayersChanged(f func(availableLayers []int32, exemptedLayers []int32)) {
	s.onAvailableLayersChanged = f
}

func (s *StreamTrackerManager) OnBitrateAvailabilityChanged(f func()) {
	s.onBitrateAvailabilityChanged = f
}

func (s *StreamTrackerManager) OnMaxLayerChanged(f func(maxLayer int32)) {
	s.onMaxLayerChanged = f
}

func (s *StreamTrackerManager) AddTracker(layer int32) *StreamTracker {
	var params StreamTrackerParams
	if s.trackInfo.Source == livekit.TrackSource_SCREEN_SHARE {
		if int(layer) >= len(ConfigScreenshare) {
			return nil
		}

		params = ConfigScreenshare[layer]
	} else {
		if int(layer) >= len(ConfigVideo) {
			return nil
		}

		params = ConfigVideo[layer]
	}
	params.Logger = logger.Logger(logr.Logger(s.logger).WithValues("layer", layer))
	tracker := NewStreamTracker(params)
	s.logger.Debugw("StreamTrackerManager add track", "layer", layer)
	tracker.OnStatusChanged(func(status StreamStatus) {
		s.logger.Debugw("StreamTrackerManager OnStatusChanged", "layer", layer, "status", status)
		if status == StreamStatusStopped {
			s.removeAvailableLayer(layer)
		} else {
			s.addAvailableLayer(layer)
		}
	})
	tracker.OnBitrateAvailable(func() {
		if s.onBitrateAvailabilityChanged != nil {
			s.onBitrateAvailabilityChanged()
		}
	})

	s.lock.Lock()
	s.trackers[layer] = tracker
	s.lock.Unlock()

	tracker.Start()
	return tracker
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
	s.availableLayers = make([]int32, 0)
	s.exemptedLayers = make([]int32, 0)
	s.maxExpectedLayer = DefaultMaxLayerSpatial
	s.paused = false
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
	s.paused = paused
	trackers := s.trackers
	s.lock.Unlock()

	for _, tracker := range trackers {
		if tracker != nil {
			tracker.SetPaused(paused)
		}
	}
}

func (s *StreamTrackerManager) SetMaxExpectedSpatialLayer(layer int32) int32 {
	s.lock.Lock()
	prev := s.maxExpectedLayer
	if layer <= s.maxExpectedLayer {
		// some higher layer(s) expected to stop, nothing else to do
		s.maxExpectedLayer = layer
		s.lock.Unlock()
		return prev
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

	return prev
}

func (s *StreamTrackerManager) DistanceToDesired() int32 {
	s.lock.RLock()
	defer s.lock.RUnlock()

	if s.paused || s.maxExpectedLayer == DefaultMaxLayerSpatial {
		return 0
	}

	if len(s.availableLayers) == 0 {
		return s.maxExpectedLayer + 1
	}

	distance := s.maxExpectedLayer - s.availableLayers[len(s.availableLayers)-1]
	if distance < 0 {
		distance = 0
	}

	return distance
}

func (s *StreamTrackerManager) GetLayerDimension(layer int32) (uint32, uint32) {
	height := uint32(0)
	width := uint32(0)
	if len(s.trackInfo.Layers) > 0 {
		quality := buffer.SpatialLayerToVideoQuality(layer, s.trackInfo)
		for _, layer := range s.trackInfo.Layers {
			if layer.Quality == quality {
				height = layer.Height
				width = layer.Width
				break
			}
		}
	} else {
		width = s.trackInfo.Width
		height = s.trackInfo.Height
	}
	return width, height
}

func (s *StreamTrackerManager) GetMaxExpectedLayer() int32 {
	s.lock.RLock()
	defer s.lock.RUnlock()

	// find min of <expected, published> layer
	maxExpectedLayer := s.maxExpectedLayer
	if maxExpectedLayer > s.maxPublishedLayer {
		maxExpectedLayer = s.maxPublishedLayer
	}
	return maxExpectedLayer
}

func (s *StreamTrackerManager) GetBitrateTemporalCumulative() Bitrates {
	s.lock.RLock()
	defer s.lock.RUnlock()

	// LK-TODO: For SVC tracks, need to accumulate across spatial layers also
	var br Bitrates

	for i, tracker := range s.trackers {
		if tracker != nil {
			tls := make([]int64, DefaultMaxLayerTemporal+1)
			if s.hasSpatialLayerLocked(int32(i)) {
				tls = tracker.BitrateTemporalCumulative()
			}

			for j := 0; j < len(br[i]); j++ {
				br[i][j] = tls[j]
			}
		}
	}

	return br
}

func (s *StreamTrackerManager) GetAvailableLayers() ([]int32, []int32) {
	s.lock.RLock()
	defer s.lock.RUnlock()

	var availableLayers []int32
	availableLayers = append(availableLayers, s.availableLayers...)

	var exemptedLayers []int32
	exemptedLayers = append(exemptedLayers, s.exemptedLayers...)

	return availableLayers, exemptedLayers
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

	for idx, el := range s.exemptedLayers {
		if el == layer {
			s.exemptedLayers[idx] = s.exemptedLayers[len(s.exemptedLayers)-1]
			s.exemptedLayers = s.exemptedLayers[:len(s.exemptedLayers)-1]
			break
		}
	}
	sort.Slice(s.exemptedLayers, func(i, j int) bool { return s.exemptedLayers[i] < s.exemptedLayers[j] })

	var availableLayers []int32
	availableLayers = append(availableLayers, s.availableLayers...)

	var exemptedLayers []int32
	exemptedLayers = append(exemptedLayers, s.exemptedLayers...)
	s.lock.Unlock()

	s.logger.Debugw(
		"available layers changed - layer seen",
		"added", layer,
		"availableLayers", availableLayers,
		"exemptedLayers", exemptedLayers,
	)

	if s.onAvailableLayersChanged != nil {
		s.onAvailableLayersChanged(availableLayers, exemptedLayers)
	}

	// check if new layer was the max layer
	if availableLayers[len(availableLayers)-1] == layer && s.onMaxLayerChanged != nil {
		s.onMaxLayerChanged(layer)
	}
}

func (s *StreamTrackerManager) removeAvailableLayer(layer int32) {
	s.lock.Lock()
	prevMaxLayer := InvalidLayerSpatial
	if len(s.availableLayers) > 0 {
		prevMaxLayer = s.availableLayers[len(s.availableLayers)-1]
	}

	//
	// remove from available if not exempt
	//
	exempt := false
	var sourceExemptedLayers []int32
	if s.trackInfo.Source == livekit.TrackSource_SCREEN_SHARE {
		sourceExemptedLayers = ExemptedLayersScreenshare
	} else {
		sourceExemptedLayers = ExemptedLayersVideo
	}
	for _, l := range sourceExemptedLayers {
		if layer == l {
			exempt = true
			break
		}
	}

	if exempt {
		if layer > s.maxExpectedLayer || s.paused {
			exempt = false
		}
	}

	newLayers := make([]int32, 0, DefaultMaxLayerSpatial+1)
	for _, l := range s.availableLayers {
		if exempt || l != layer {
			newLayers = append(newLayers, l)
		}
	}
	sort.Slice(newLayers, func(i, j int) bool { return newLayers[i] < newLayers[j] })
	s.availableLayers = newLayers

	//
	// add to exempt if not already present
	//
	if exempt {
		found := false
		for _, el := range s.exemptedLayers {
			if el == layer {
				found = true
				break
			}
		}
		if !found {
			s.exemptedLayers = append(s.exemptedLayers, layer)
			sort.Slice(s.exemptedLayers, func(i, j int) bool { return s.exemptedLayers[i] < s.exemptedLayers[j] })
		}
	}

	var exemptedLayers []int32
	exemptedLayers = append(exemptedLayers, s.exemptedLayers...)

	curMaxLayer := InvalidLayerSpatial
	if len(s.availableLayers) > 0 {
		curMaxLayer = s.availableLayers[len(s.availableLayers)-1]
	}
	s.lock.Unlock()

	s.logger.Debugw(
		"available layers changed - layer gone",
		"removed", layer,
		"availableLayers", newLayers,
		"exeptedLayers", exemptedLayers,
	)

	// need to immediately switch off unavailable layers
	if s.onAvailableLayersChanged != nil {
		s.onAvailableLayersChanged(newLayers, exemptedLayers)
	}

	// if maxLayer was removed, send the new maxLayer
	if curMaxLayer != prevMaxLayer && s.onMaxLayerChanged != nil {
		s.onMaxLayerChanged(curMaxLayer)
	}
}

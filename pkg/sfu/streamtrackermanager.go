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

package sfu

import (
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/frostbyte73/core"
	"go.uber.org/atomic"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/mime"
	"github.com/livekit/livekit-server/pkg/sfu/streamtracker"
)

// ---------------------------------------------------

type StreamTrackerManagerListener interface {
	OnAvailableLayersChanged()
	OnBitrateAvailabilityChanged()
	OnMaxPublishedLayerChanged(maxPublishedLayer int32)
	OnMaxTemporalLayerSeenChanged(maxTemporalLayerSeen int32)
	OnMaxAvailableLayerChanged(maxAvailableLayer int32)
	OnBitrateReport(availableLayers []int32, bitrates Bitrates)
}

// ---------------------------------------------------

type (
	StreamTrackerType string
)

const (
	StreamTrackerTypePacket StreamTrackerType = "packet"
	StreamTrackerTypeFrame  StreamTrackerType = "frame"
)

type StreamTrackerPacketConfig struct {
	SamplesRequired uint32        `yaml:"samples_required,omitempty"` // number of samples needed per cycle
	CyclesRequired  uint32        `yaml:"cycles_required,omitempty"`  // number of cycles needed to be active
	CycleDuration   time.Duration `yaml:"cycle_duration,omitempty"`
}

type StreamTrackerFrameConfig struct {
	MinFPS float64 `yaml:"min_fps,omitempty"`
}

type StreamTrackerConfig struct {
	StreamTrackerType     StreamTrackerType                                 `yaml:"stream_tracker_type,omitempty"`
	BitrateReportInterval map[int32]time.Duration                           `yaml:"bitrate_report_interval,omitempty"`
	PacketTracker         map[int32]streamtracker.StreamTrackerPacketConfig `yaml:"packet_tracker,omitempty"`
	FrameTracker          map[int32]streamtracker.StreamTrackerFrameConfig  `yaml:"frame_tracker,omitempty"`
}

var (
	DefaultStreamTrackerConfigVideo = StreamTrackerConfig{
		StreamTrackerType: StreamTrackerTypePacket,
		BitrateReportInterval: map[int32]time.Duration{
			0: 1 * time.Second,
			1: 1 * time.Second,
			2: 1 * time.Second,
		},
		PacketTracker: streamtracker.DefaultStreamTrackerPacketConfigVideo,
		FrameTracker:  streamtracker.DefaultStreamTrackerFrameConfigVideo,
	}

	DefaultStreamTrackerConfigScreenshare = StreamTrackerConfig{
		StreamTrackerType: StreamTrackerTypePacket,
		BitrateReportInterval: map[int32]time.Duration{
			0: 4 * time.Second,
			1: 4 * time.Second,
			2: 4 * time.Second,
		},
		PacketTracker: streamtracker.DefaultStreamTrackerPacketConfigScreenshare,
		FrameTracker:  streamtracker.DefaultStreamTrackerFrameConfigScreenshare,
	}
)

// ---------------------------------------------------

type StreamTrackerManagerConfig struct {
	Video       StreamTrackerConfig `yaml:"video,omitempty"`
	Screenshare StreamTrackerConfig `yaml:"screenshare,omitempty"`
}

var (
	DefaultStreamTrackerManagerConfig = StreamTrackerManagerConfig{
		Video:       DefaultStreamTrackerConfigVideo,
		Screenshare: DefaultStreamTrackerConfigScreenshare,
	}
)

// ---------------------------------------------------

type StreamTrackerManager struct {
	logger         logger.Logger
	trackInfo      atomic.Pointer[livekit.TrackInfo]
	mimeType       mime.MimeType
	videoLayerMode livekit.VideoLayer_Mode
	clockRate      uint32

	trackerConfig StreamTrackerConfig

	lock                 sync.RWMutex
	maxPublishedLayer    int32
	maxTemporalLayerSeen int32

	ddTracker *streamtracker.StreamTrackerDependencyDescriptor
	trackers  [buffer.DefaultMaxLayerSpatial + 1]streamtracker.StreamTrackerWorker

	availableLayers  []int32
	maxExpectedLayer int32
	paused           bool

	closed core.Fuse

	listener StreamTrackerManagerListener
}

func NewStreamTrackerManager(
	logger logger.Logger,
	trackInfo *livekit.TrackInfo,
	mimeType mime.MimeType,
	clockRate uint32,
	config StreamTrackerManagerConfig,
) *StreamTrackerManager {
	s := &StreamTrackerManager{
		logger:               logger,
		mimeType:             mimeType,
		videoLayerMode:       buffer.GetVideoLayerModeForMimeType(mimeType, trackInfo),
		maxPublishedLayer:    buffer.InvalidLayerSpatial,
		maxTemporalLayerSeen: buffer.InvalidLayerTemporal,
		clockRate:            clockRate,
	}
	s.trackInfo.Store(utils.CloneProto(trackInfo))

	switch trackInfo.Source {
	case livekit.TrackSource_SCREEN_SHARE:
		s.trackerConfig = config.Screenshare
	case livekit.TrackSource_CAMERA:
		s.trackerConfig = config.Video
	default:
		s.trackerConfig = config.Video
	}

	s.maxExpectedLayerFromTrackInfo()

	if trackInfo.Type == livekit.TrackType_VIDEO {
		go s.bitrateReporter()
	}
	return s
}

func (s *StreamTrackerManager) Close() {
	s.closed.Break()
}

func (s *StreamTrackerManager) SetListener(listener StreamTrackerManagerListener) {
	s.lock.Lock()
	s.listener = listener
	s.lock.Unlock()
}

func (s *StreamTrackerManager) getListener() StreamTrackerManagerListener {
	s.lock.RLock()
	defer s.lock.RUnlock()

	return s.listener
}

func (s *StreamTrackerManager) createStreamTrackerPacket(layer int32) streamtracker.StreamTrackerImpl {
	packetTrackerConfig, ok := s.trackerConfig.PacketTracker[layer]
	if !ok {
		return nil
	}

	params := streamtracker.StreamTrackerPacketParams{
		Config: packetTrackerConfig,
		Logger: s.logger.WithValues("layer", layer),
	}
	return streamtracker.NewStreamTrackerPacket(params)
}

func (s *StreamTrackerManager) createStreamTrackerFrame(layer int32) streamtracker.StreamTrackerImpl {
	frameTrackerConfig, ok := s.trackerConfig.FrameTracker[layer]
	if !ok {
		return nil
	}

	params := streamtracker.StreamTrackerFrameParams{
		Config:    frameTrackerConfig,
		ClockRate: s.clockRate,
		Logger:    s.logger.WithValues("layer", layer),
	}
	return streamtracker.NewStreamTrackerFrame(params)
}

func (s *StreamTrackerManager) AddDependencyDescriptorTrackers() {
	bitrateInterval, ok := s.trackerConfig.BitrateReportInterval[0]
	if !ok {
		return
	}
	s.lock.Lock()
	var addAllTrackers bool
	if s.ddTracker == nil {
		s.ddTracker = streamtracker.NewStreamTrackerDependencyDescriptor(streamtracker.StreamTrackerParams{
			BitrateReportInterval: bitrateInterval,
			Logger:                s.logger.WithValues("layer", 0),
		})
		addAllTrackers = true
	}
	s.lock.Unlock()
	if addAllTrackers {
		for i := 0; i <= int(buffer.DefaultMaxLayerSpatial); i++ {
			s.AddTracker(int32(i))
		}
	}
}

func (s *StreamTrackerManager) AddTracker(layer int32) streamtracker.StreamTrackerWorker {
	if layer < 0 || int(layer) >= len(s.trackers) {
		return nil
	}

	var tracker streamtracker.StreamTrackerWorker
	s.lock.Lock()
	tracker = s.trackers[layer]
	if tracker != nil {
		s.lock.Unlock()
		return tracker
	}

	if s.ddTracker != nil {
		tracker = s.ddTracker.LayeredTracker(layer)
	}
	s.lock.Unlock()

	bitrateInterval, ok := s.trackerConfig.BitrateReportInterval[layer]
	if !ok {
		return nil
	}

	if tracker == nil {
		var trackerImpl streamtracker.StreamTrackerImpl
		switch s.trackerConfig.StreamTrackerType {
		case StreamTrackerTypePacket:
			trackerImpl = s.createStreamTrackerPacket(layer)
		case StreamTrackerTypeFrame:
			trackerImpl = s.createStreamTrackerFrame(layer)
		}
		if trackerImpl == nil {
			return nil
		}

		tracker = streamtracker.NewStreamTracker(streamtracker.StreamTrackerParams{
			StreamTrackerImpl:     trackerImpl,
			BitrateReportInterval: bitrateInterval,
			Logger:                s.logger.WithValues("layer", layer),
		})
	}

	s.logger.Debugw("stream tracker add track", "layer", layer)
	tracker.OnStatusChanged(func(status streamtracker.StreamStatus) {
		s.logger.Debugw("stream tracker status changed", "layer", layer, "status", status)
		if status == streamtracker.StreamStatusStopped {
			s.removeAvailableLayer(layer)
		} else {
			s.addAvailableLayer(layer)
		}
	})
	tracker.OnBitrateAvailable(func() {
		if listener := s.getListener(); listener != nil {
			listener.OnBitrateAvailabilityChanged()
		}
	})

	s.lock.Lock()
	paused := s.paused
	s.trackers[layer] = tracker

	notify := false
	if layer > s.maxPublishedLayer {
		s.maxPublishedLayer = layer
		notify = true
	}
	s.lock.Unlock()

	if notify {
		if listener := s.getListener(); listener != nil {
			go listener.OnMaxPublishedLayerChanged(layer)
		}
	}

	tracker.SetPaused(paused)
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
	s.maxExpectedLayerFromTrackInfoLocked()
	s.paused = false
	ddTracker := s.ddTracker
	s.ddTracker = nil
	s.lock.Unlock()

	for _, tracker := range trackers {
		if tracker != nil {
			tracker.Stop()
		}
	}
	if ddTracker != nil {
		ddTracker.Stop()
	}
}

func (s *StreamTrackerManager) GetTracker(layer int32) streamtracker.StreamTrackerWorker {
	s.lock.RLock()
	defer s.lock.RUnlock()

	if layer < 0 || int(layer) >= len(s.trackers) {
		s.logger.Errorw("unexpected layer", nil, "layer", layer)
		return nil
	}
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

func (s *StreamTrackerManager) IsPaused() bool {
	s.lock.RLock()
	defer s.lock.RUnlock()

	return s.paused
}

func (s *StreamTrackerManager) UpdateTrackInfo(ti *livekit.TrackInfo) {
	s.trackInfo.Store(utils.CloneProto(ti))
	s.maxExpectedLayerFromTrackInfo()
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
	// If the layer was not detected as stopped (i.e. it is still in available layers),
	// resetting tracker will declare layer available afresh. That's fine as it will be
	// a no-op in available layers handling.
	//
	var trackersToReset []streamtracker.StreamTrackerWorker
	for l := s.maxExpectedLayer + 1; l <= layer; l++ {
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

func (s *StreamTrackerManager) DistanceToDesired() float64 {
	s.lock.RLock()
	defer s.lock.RUnlock()

	if s.paused || s.maxExpectedLayer < 0 || s.maxTemporalLayerSeen < 0 {
		return 0
	}

	al, brs := s.getLayeredBitrateLocked()

	maxLayer := buffer.InvalidLayer
done:
	for s := int32(len(brs)) - 1; s >= 0; s-- {
		for t := int32(len(brs[0])) - 1; t >= 0; t-- {
			if brs[s][t] != 0 {
				maxLayer = buffer.VideoLayer{
					Spatial:  s,
					Temporal: t,
				}
				break done
			}
		}
	}

	// before bit rate measurement is available, stream tracker could declare layer seen, account for that
	for _, layer := range al {
		if layer > maxLayer.Spatial {
			maxLayer.Spatial = layer
			maxLayer.Temporal = s.maxTemporalLayerSeen // till bit rate measurement is available, assume max seen as temporal
		}
	}

	adjustedMaxLayers := maxLayer
	if !maxLayer.IsValid() {
		adjustedMaxLayers = buffer.VideoLayer{Spatial: 0, Temporal: 0}
	}

	distance :=
		((s.maxExpectedLayer - adjustedMaxLayers.Spatial) * (s.maxTemporalLayerSeen + 1)) +
			(s.maxTemporalLayerSeen - adjustedMaxLayers.Temporal)
	if !maxLayer.IsValid() {
		distance++
	}

	return float64(distance) / float64(s.maxTemporalLayerSeen+1)
}

func (s *StreamTrackerManager) GetMaxPublishedLayer() int32 {
	s.lock.RLock()
	defer s.lock.RUnlock()

	return s.maxPublishedLayer
}

func (s *StreamTrackerManager) GetLayeredBitrate() ([]int32, Bitrates) {
	s.lock.RLock()
	defer s.lock.RUnlock()

	return s.getLayeredBitrateLocked()
}

func (s *StreamTrackerManager) getLayeredBitrateLocked() ([]int32, Bitrates) {
	var br Bitrates

	for i, tracker := range s.trackers {
		if tracker != nil {
			tls := make([]int64, buffer.DefaultMaxLayerTemporal+1)
			if slices.Contains(s.availableLayers, int32(i)) {
				tls = tracker.BitrateTemporalCumulative()
			}

			for j := 0; j < len(br[i]); j++ {
				br[i][j] = tls[j]
			}
		}
	}

	// accumulate bitrates for SVC streams without dependency descriptor
	if s.videoLayerMode == livekit.VideoLayer_MULTIPLE_SPATIAL_LAYERS_PER_STREAM && s.ddTracker == nil {
		for i := len(br) - 1; i >= 1; i-- {
			for j := len(br[i]) - 1; j >= 0; j-- {
				if br[i][j] != 0 {
					for k := i - 1; k >= 0; k-- {
						br[i][j] += br[k][j]
					}
				}
			}
		}
	}

	availableLayers := make([]int32, len(s.availableLayers))
	copy(availableLayers, s.availableLayers)

	return availableLayers, br
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

	// check if new layer is the max layer
	isMaxLayerChange := s.availableLayers[len(s.availableLayers)-1] == layer

	s.logger.Debugw(
		"available layers changed - layer seen",
		"added", layer,
		"availableLayers", s.availableLayers,
	)
	s.lock.Unlock()

	if listener := s.getListener(); listener != nil {
		listener.OnAvailableLayersChanged()

		if isMaxLayerChange {
			listener.OnMaxAvailableLayerChanged(layer)
		}
	}
}

func (s *StreamTrackerManager) removeAvailableLayer(layer int32) {
	s.lock.Lock()
	prevMaxLayer := buffer.InvalidLayerSpatial
	if len(s.availableLayers) > 0 {
		prevMaxLayer = s.availableLayers[len(s.availableLayers)-1]
	}

	newLayers := make([]int32, 0, buffer.DefaultMaxLayerSpatial+1)
	for _, l := range s.availableLayers {
		if l != layer {
			newLayers = append(newLayers, l)
		}
	}
	sort.Slice(newLayers, func(i, j int) bool { return newLayers[i] < newLayers[j] })
	s.availableLayers = newLayers

	s.logger.Debugw(
		"available layers changed - layer gone",
		"removed", layer,
		"availableLayers", newLayers,
	)

	curMaxLayer := buffer.InvalidLayerSpatial
	if len(s.availableLayers) > 0 {
		curMaxLayer = s.availableLayers[len(s.availableLayers)-1]
	}
	s.lock.Unlock()

	// need to immediately switch off unavailable layers
	if listener := s.getListener(); listener != nil {
		listener.OnAvailableLayersChanged()

		// if maxLayer was removed, send the new maxLayer
		if curMaxLayer != prevMaxLayer {
			listener.OnMaxAvailableLayerChanged(curMaxLayer)
		}
	}
}

func (s *StreamTrackerManager) maxExpectedLayerFromTrackInfo() {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.maxExpectedLayerFromTrackInfoLocked()
}

func (s *StreamTrackerManager) maxExpectedLayerFromTrackInfoLocked() {
	s.maxExpectedLayer = buffer.InvalidLayerSpatial
	ti := s.trackInfo.Load()
	if ti != nil {
		for _, layer := range buffer.GetVideoLayersForMimeType(s.mimeType, ti) {
			if layer.SpatialLayer > s.maxExpectedLayer {
				s.maxExpectedLayer = layer.SpatialLayer
			}
		}
	}
}

func (s *StreamTrackerManager) GetMaxTemporalLayerSeen() int32 {
	s.lock.RLock()
	defer s.lock.RUnlock()

	return s.maxTemporalLayerSeen
}

func (s *StreamTrackerManager) updateMaxTemporalLayerSeen(brs Bitrates) {
	maxTemporalLayerSeen := buffer.InvalidLayerTemporal
done:
	for t := int32(len(brs[0])) - 1; t >= 0; t-- {
		for s := int32(len(brs)) - 1; s >= 0; s-- {
			if brs[s][t] != 0 {
				maxTemporalLayerSeen = t
				break done
			}
		}
	}

	s.lock.Lock()
	if maxTemporalLayerSeen <= s.maxTemporalLayerSeen {
		s.lock.Unlock()
		return
	}

	s.maxTemporalLayerSeen = maxTemporalLayerSeen
	s.lock.Unlock()

	if listener := s.getListener(); listener != nil {
		listener.OnMaxTemporalLayerSeenChanged(maxTemporalLayerSeen)
	}
}

func (s *StreamTrackerManager) bitrateReporter() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-s.closed.Watch():
			return

		case <-ticker.C:
			al, brs := s.GetLayeredBitrate()
			s.updateMaxTemporalLayerSeen(brs)

			if listener := s.getListener(); listener != nil {
				listener.OnBitrateReport(al, brs)
			}
		}
	}
}

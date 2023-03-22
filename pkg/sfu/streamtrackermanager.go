package sfu

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/frostbyte73/core"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/streamtracker"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

type StreamTrackerManagerListener interface {
	OnAvailableLayersChanged()
	OnBitrateAvailabilityChanged()
	OnMaxPublishedLayerChanged(maxPublishedLayer int32)
	OnMaxTemporalLayerSeenChanged(maxTemporalLayerSeen int32)
	OnMaxAvailableLayerChanged(maxAvailableLayer int32)
	OnBitrateReport(availableLayers []int32, bitrates Bitrates)
}

type StreamTrackerManager struct {
	logger    logger.Logger
	trackInfo *livekit.TrackInfo
	isSVC     bool
	clockRate uint32

	trackerConfig config.StreamTrackerConfig

	lock                 sync.RWMutex
	maxPublishedLayer    int32
	maxTemporalLayerSeen int32

	trackers [DefaultMaxLayerSpatial + 1]*streamtracker.StreamTracker

	availableLayers  []int32
	maxExpectedLayer int32
	paused           bool

	senderReportMu sync.RWMutex
	senderReports  [DefaultMaxLayerSpatial + 1]*buffer.RTCPSenderReportDataExt

	closed core.Fuse

	listener StreamTrackerManagerListener
}

func NewStreamTrackerManager(
	logger logger.Logger,
	trackInfo *livekit.TrackInfo,
	isSVC bool,
	clockRate uint32,
	trackersConfig config.StreamTrackersConfig,
) *StreamTrackerManager {
	s := &StreamTrackerManager{
		logger:               logger,
		trackInfo:            trackInfo,
		isSVC:                isSVC,
		maxPublishedLayer:    InvalidLayerSpatial,
		maxTemporalLayerSeen: InvalidLayerTemporal,
		clockRate:            clockRate,
		closed:               core.NewFuse(),
	}

	switch s.trackInfo.Source {
	case livekit.TrackSource_SCREEN_SHARE:
		s.trackerConfig = trackersConfig.Screenshare
	case livekit.TrackSource_CAMERA:
		s.trackerConfig = trackersConfig.Video
	default:
		s.trackerConfig = trackersConfig.Video
	}

	s.maxExpectedLayerFromTrackInfo()

	if s.trackInfo.Type == livekit.TrackType_VIDEO {
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

func (s *StreamTrackerManager) AddTracker(layer int32) *streamtracker.StreamTracker {
	bitrateInterval, ok := s.trackerConfig.BitrateReportInterval[layer]
	if !ok {
		return nil
	}

	var trackerImpl streamtracker.StreamTrackerImpl
	switch s.trackerConfig.StreamTrackerType {
	case config.StreamTrackerTypePacket:
		trackerImpl = s.createStreamTrackerPacket(layer)
	case config.StreamTrackerTypeFrame:
		trackerImpl = s.createStreamTrackerFrame(layer)
	}
	if trackerImpl == nil {
		return nil
	}

	tracker := streamtracker.NewStreamTracker(streamtracker.StreamTrackerParams{
		StreamTrackerImpl:     trackerImpl,
		BitrateReportInterval: bitrateInterval,
		Logger:                s.logger.WithValues("layer", layer),
	})

	s.logger.Debugw("StreamTrackerManager add track", "layer", layer)
	tracker.OnStatusChanged(func(status streamtracker.StreamStatus) {
		s.logger.Debugw("StreamTrackerManager OnStatusChanged", "layer", layer, "status", status)
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
	s.maxExpectedLayerFromTrackInfo()
	s.paused = false
	s.lock.Unlock()

	for _, tracker := range trackers {
		if tracker != nil {
			tracker.Stop()
		}
	}
}

func (s *StreamTrackerManager) GetTracker(layer int32) *streamtracker.StreamTracker {
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

func (s *StreamTrackerManager) IsPaused() bool {
	s.lock.RLock()
	defer s.lock.RUnlock()

	return s.paused
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
	// don't need to do anything. If not, reset the stream tracker so that
	// the layer is declared available on the first packet.
	//
	// NOTE: There may be a race between checking if a layer is available and
	// resetting the tracker, i.e. the track may stop just after checking.
	// But, those conditions should be rare. In those cases, the restart will
	// take longer.
	//
	var trackersToReset []*streamtracker.StreamTracker
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

func (s *StreamTrackerManager) DistanceToDesired() float64 {
	s.lock.RLock()
	defer s.lock.RUnlock()

	if s.paused || s.maxExpectedLayer < 0 || s.maxTemporalLayerSeen < 0 {
		return 0
	}

	al, brs := s.getLayeredBitrateLocked()

	maxLayers := InvalidLayers
done:
	for s := int32(len(brs)) - 1; s >= 0; s-- {
		for t := int32(len(brs[0])) - 1; t >= 0; t-- {
			if brs[s][t] != 0 {
				maxLayers = VideoLayers{
					Spatial:  s,
					Temporal: t,
				}
				break done
			}
		}
	}

	// before bit rate measurement is available, stream tracker could declare layer seen, account for that
	for _, layer := range al {
		if layer > maxLayers.Spatial {
			maxLayers.Spatial = layer
			maxLayers.Temporal = s.maxTemporalLayerSeen // till bit rate measurement is available, assume max seen as temporal
		}
	}

	adjustedMaxLayers := maxLayers
	if !maxLayers.IsValid() {
		adjustedMaxLayers = VideoLayers{Spatial: 0, Temporal: 0}
	}

	distance :=
		((s.maxExpectedLayer - adjustedMaxLayers.Spatial) * (s.maxTemporalLayerSeen + 1)) +
			(s.maxTemporalLayerSeen - adjustedMaxLayers.Temporal)
	if !maxLayers.IsValid() {
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
			tls := make([]int64, DefaultMaxLayerTemporal+1)
			if s.hasSpatialLayerLocked(int32(i)) {
				tls = tracker.BitrateTemporalCumulative()
			}

			for j := 0; j < len(br[i]); j++ {
				br[i][j] = tls[j]
			}
		}
	}

	if s.isSVC {
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
	prevMaxLayer := InvalidLayerSpatial
	if len(s.availableLayers) > 0 {
		prevMaxLayer = s.availableLayers[len(s.availableLayers)-1]
	}

	newLayers := make([]int32, 0, DefaultMaxLayerSpatial+1)
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

	curMaxLayer := InvalidLayerSpatial
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
	s.maxExpectedLayer = InvalidLayerSpatial
	for _, layer := range s.trackInfo.Layers {
		spatialLayer := buffer.VideoQualityToSpatialLayer(layer.Quality, s.trackInfo)
		if spatialLayer > s.maxExpectedLayer {
			s.maxExpectedLayer = spatialLayer
		}
	}
}

func (s *StreamTrackerManager) SetRTCPSenderReportDataExt(layer int32, senderReport *buffer.RTCPSenderReportDataExt) {
	s.senderReportMu.Lock()
	defer s.senderReportMu.Unlock()

	if layer < 0 || int(layer) >= len(s.senderReports) {
		return
	}

	s.senderReports[layer] = senderReport
}

func (s *StreamTrackerManager) GetRTCPSenderReportDataExt(layer int32) *buffer.RTCPSenderReportDataExt {
	s.senderReportMu.RLock()
	defer s.senderReportMu.RUnlock()

	if layer < 0 || int(layer) >= len(s.senderReports) {
		return nil
	}

	return s.senderReports[layer]
}

func (s *StreamTrackerManager) GetReferenceLayerRTPTimestamp(ts uint32, layer int32, referenceLayer int32) (uint32, error) {
	s.senderReportMu.RLock()
	defer s.senderReportMu.RUnlock()

	if layer < 0 || referenceLayer < 0 {
		return 0, fmt.Errorf("invalid layer, target: %d, reference: %d", layer, referenceLayer)
	}

	if layer == referenceLayer {
		return ts, nil
	}

	var srLayer *buffer.RTCPSenderReportDataExt
	if int(layer) < len(s.senderReports) {
		srLayer = s.senderReports[layer]
	}
	if srLayer == nil || srLayer.SenderReportData.NTPTimestamp == 0 {
		return 0, fmt.Errorf("layer rtcp sender report not available: %d", layer)
	}

	var srRef *buffer.RTCPSenderReportDataExt
	if int(referenceLayer) < len(s.senderReports) {
		srRef = s.senderReports[referenceLayer]
	}
	if srRef == nil || srRef.SenderReportData.NTPTimestamp == 0 {
		return 0, fmt.Errorf("reference layer rtcp sender report not available: %d", referenceLayer)
	}

	// line up the RTP time stamps using NTP time of most recent sender report of layer and referenceLayer
	// NOTE: It is possible that reference layer has stopped (due to dynacast/adaptive streaming OR publisher
	// constraints). It should be okay even if the layer has stopped for a long time when using modulo arithmetic for
	// RTP time stamp (uint32 arithmetic).
	ntpDiff := float64(int64(srRef.SenderReportData.NTPTimestamp-srLayer.SenderReportData.NTPTimestamp)) / float64(1<<32)
	normalizedTS := srLayer.SenderReportData.RTPTimestamp + uint32(ntpDiff*float64(s.clockRate))

	// now that both RTP timestamps correspond to roughly the same NTP time,
	// the diff between them is the offset in RTP timestamp units between layer and referenceLayer.
	// Add the offset to layer's ts to map it to corresponding RTP timestamp in
	// the reference layer.
	return ts + (srRef.SenderReportData.RTPTimestamp - normalizedTS), nil
}

func (s *StreamTrackerManager) GetMaxTemporalLayerSeen() int32 {
	s.lock.RLock()
	defer s.lock.RUnlock()

	return s.maxTemporalLayerSeen
}

func (s *StreamTrackerManager) updateMaxTemporalLayerSeen(brs Bitrates) {
	maxTemporalLayerSeen := InvalidLayerTemporal
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

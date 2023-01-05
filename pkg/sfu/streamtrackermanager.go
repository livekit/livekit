package sfu

import (
	"fmt"
	"sort"
	"sync"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/streamtracker"
	"github.com/livekit/mediatransportutil"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

// ---------------------------------------------------------

type rtcpSenderReportData struct {
	rtpTS uint32
	ntpTS mediatransportutil.NtpTime
}

func (r *rtcpSenderReportData) String() string {
	return fmt.Sprintf("rtpTS: %d, ntpTS: +%v", r.rtpTS, r.ntpTS.Time())
}

// ---------------------------------------------------------

type StreamTrackerManager struct {
	logger            logger.Logger
	trackInfo         *livekit.TrackInfo
	isSVC             bool
	maxPublishedLayer int32
	clockRate         uint32

	trackerConfig config.StreamTrackerConfig

	lock sync.RWMutex

	trackers      [DefaultMaxLayerSpatial + 1]*streamtracker.StreamTracker
	senderReports [DefaultMaxLayerSpatial + 1]*rtcpSenderReportData

	availableLayers  []int32
	exemptedLayers   []int32
	maxExpectedLayer int32
	paused           bool

	onAvailableLayersChanged     func(availableLayers []int32, exemptedLayers []int32)
	onBitrateAvailabilityChanged func()
	onMaxLayerChanged            func(maxLayer int32)
}

func NewStreamTrackerManager(
	logger logger.Logger,
	trackInfo *livekit.TrackInfo,
	isSVC bool,
	clockRate uint32,
	trackersConfig config.StreamTrackersConfig,
) *StreamTrackerManager {
	s := &StreamTrackerManager{
		logger:            logger,
		trackInfo:         trackInfo,
		isSVC:             isSVC,
		maxPublishedLayer: 0,
		clockRate:         clockRate,
	}

	switch s.trackInfo.Source {
	case livekit.TrackSource_SCREEN_SHARE:
		s.trackerConfig = trackersConfig.Screenshare
	case livekit.TrackSource_CAMERA:
		s.trackerConfig = trackersConfig.Video
	default:
		s.trackerConfig = trackersConfig.Video
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
		if s.onBitrateAvailabilityChanged != nil {
			s.onBitrateAvailabilityChanged()
		}
	})

	s.lock.Lock()
	paused := s.paused
	s.trackers[layer] = tracker
	s.lock.Unlock()

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
	// If the layer was not stopped (i.e. it will still be in available layers),
	// don't need to do anything. If not, reset the stream tracker so that
	// the layer is declared available on the first packet
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

func (s *StreamTrackerManager) GetLayeredBitrate() Bitrates {
	s.lock.RLock()
	defer s.lock.RUnlock()

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

	s.logger.Infow(
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
	for _, l := range s.trackerConfig.ExemptedLayers {
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
		// do not remove layers for non-simulcast
		if exempt || l != layer || len(s.trackInfo.Layers) < 2 {
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

	s.logger.Infow(
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

/* RAJA-REMOVE
func (s *StreamTrackerManager) UpdateRTCPSenderReportData(layer int32, rtpTS uint32, ntpTS mediatransportutil.NtpTime) {
	s.lock.Lock()
	defer s.lock.Unlock()

	if layer == InvalidLayerSpatial || int(layer) >= len(s.senderReports) || ntpTS == 0 {
		return
	}

	sr := s.senderReports[layer]
	if sr == nil {
		s.senderReports[layer] = &rtcpSenderReportData{
			rtpTS: rtpTS,
			ntpTS: ntpTS,
		}
		s.logger.Infow("RAJA sr data", "layer", layer, "rtpTS", rtpTS, "ntpTS", ntpTS.Time(), "ntpTSRaw", ntpTS, "seconds", ntpTS>>32) // REMOVE
	} else {
		s.logger.Infow("RAJA sr data update", "layer", layer, "rtpTS", rtpTS, "diff", rtpTS-sr.rtpTS, "ntpTS", ntpTS.Time(), "ntpTSRaw", ntpTS, "seconds", ntpTS>>32, "diffNTP", ntpTS.Time().Sub(sr.ntpTS.Time())) // REMOVE
		sr.rtpTS = rtpTS
		sr.ntpTS = ntpTS
	}
}

func (s *StreamTrackerManager) GetRTCPSenderReportData(layer int32) (rtpTS uint32, ntpTS mediatransportutil.NtpTime, err error) {
	s.lock.RLock()
	defer s.lock.RUnlock()

	if layer == InvalidLayerSpatial || int(layer) >= len(s.senderReports) {
		err = fmt.Errorf("invalid layer: %d", layer)
		return
	}

	sr := s.senderReports[layer]
	if sr != nil {
		rtpTS = sr.rtpTS
		ntpTS = sr.ntpTS
	} else {
		err = fmt.Errorf("unavailable layer: %d", layer)
	}
	return
}

func (s *StreamTrackerManager) GetReferenceLayerRTPTimestamp(ts uint32, layer int32, referenceLayer int32) (uint32, error) {
	s.lock.RLock()
	defer s.lock.RUnlock()

	if layer == referenceLayer {
		return 0, nil
	}

	if layer == InvalidLayerSpatial || int(layer) >= len(s.senderReports) {
		return 0, fmt.Errorf("invalid layer: %d", layer)
	}
	srLayer := s.senderReports[layer]
	if srLayer == nil || srLayer.ntpTS == 0 {
		return 0, fmt.Errorf("layer rtcp sender report not available: %d", layer)
	}

	if referenceLayer == InvalidLayerSpatial || int(referenceLayer) >= len(s.senderReports) {
		return 0, fmt.Errorf("invalid reference layer: %d", referenceLayer)
	}
	srRef := s.senderReports[referenceLayer]
	if srRef == nil || srRef.ntpTS == 0 {
		return 0, fmt.Errorf("reference layer rtcp sender report not available: %d", referenceLayer)
	}

	// using NTP time of most recent sender report of layer and referenceLayer
	// line up the RTP time stamps
	ntpDiff := float64(int64(srRef.ntpTS-srLayer.ntpTS)) / float64(1<<32)
	normalizedReqTS := srLayer.rtpTS + uint32(ntpDiff*float64(s.clockRate))

	// now that both RTP timestamp correspond to roughly the same NTP time,
	// the diff is the offset in RTP timestamps between layer and referenceLayer.
	// Add the offset to layer's ts to map it to corresponding RTP timestamp in
	// the reference layer.
	s.logger.Infow("RAJA normalized TS", "ts", ts, "reqL", layer, "srLayer", srLayer, "refL", referenceLayer, "srRef", srRef, "ntpDiff", ntpDiff, "normalizedReqTS", normalizedReqTS, "adjusted", ts+(srRef.rtpTS-normalizedReqTS)) // REMOVE
	return ts + (srRef.rtpTS - normalizedReqTS), nil
}
*/

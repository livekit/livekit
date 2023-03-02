package connectionquality

import (
	"strings"
	"sync"
	"time"

	"go.uber.org/atomic"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/pion/webrtc/v3"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
)

const (
	UpdateInterval           = 5 * time.Second
	audioPacketRateThreshold = float64(25.0)
)

type ConnectionStatsParams struct {
	UpdateInterval         time.Duration
	MimeType               string
	IsFECEnabled           bool // RAJA-TODO: this needs to be passed in
	GetDeltaStats          func() map[uint32]*buffer.StreamStatsWithLayers
	GetMaxExpectedLayer    func() int32
	GetCurrentLayerSpatial func() int32
	GetIsReducedQuality    func() (int32, bool)
	Logger                 logger.Logger
}

type ConnectionStats struct {
	params    ConnectionStatsParams
	codecName string // RAJA-REMOVE
	trackInfo *livekit.TrackInfo

	onStatsUpdate func(cs *ConnectionStats, stat *livekit.AnalyticsStat)

	scorer *qualityScore

	lock             sync.RWMutex
	score            float32   // RAJA-REMOVE
	lastUpdate       time.Time // RAJA-REMOVE
	maxExpectedLayer int32

	// RAJA-REMOVE done     chan struct{}
	isClosed atomic.Bool

	// RAJA-TODO
	//   - mute/unmute time
	//   - max expected layer change time
	//   - forwarder target layer switch time
}

func NewConnectionStats(params ConnectionStatsParams) *ConnectionStats {
	return &ConnectionStats{
		params: params,
		scorer: newQualityScore(qualityScoreParams{
			PacketLossWeight: getPacketLossWeight(params.MimeType, params.IsFECEnabled), // LK-TODO: have to notify codec change?
		}),
		codecName:        getCodecNameFromMime(params.MimeType), // LK-TODO: have to notify on codec change
		score:            MaxScore,
		maxExpectedLayer: buffer.InvalidLayerSpatial,
		// RAJA-REMOVE done:             make(chan struct{}),
	}
}

func (cs *ConnectionStats) Start(trackInfo *livekit.TrackInfo) {
	cs.lock.Lock()
	cs.trackInfo = trackInfo

	cs.lock.Unlock()

	go cs.updateStatsWorker()
}

func (cs *ConnectionStats) Close() {
	/* RAJA-REMOVE
	if cs.isClosed.Swap(true) {
		return
	}

	close(cs.done)
	*/
	cs.isClosed.Store(true)
}

func (cs *ConnectionStats) OnStatsUpdate(fn func(cs *ConnectionStats, stat *livekit.AnalyticsStat)) {
	cs.onStatsUpdate = fn
}

func (cs *ConnectionStats) GetScore() float32 {
	cs.lock.RLock()
	defer cs.lock.RUnlock()

	return cs.score
}

func (cs *ConnectionStats) getLayerDimensions(layer int32) (uint32, uint32) {
	if cs.trackInfo == nil {
		return 0, 0
	}

	for _, l := range cs.trackInfo.Layers {
		if layer == buffer.VideoQualityToSpatialLayer(l.Quality, cs.trackInfo) {
			return l.Width, l.Height
		}
	}

	return 0, 0
}

/* RAJA-REMOVE
type windowStat struct {
	startTime       time.Time
	duration        time.Duration
	packetsExpected uint32
	packetsLost     uint32
	rttMax          uint32
	jitterMax       float64
}
*/

func (cs *ConnectionStats) updateScore(streams map[uint32]*buffer.StreamStatsWithLayers) float32 {
	// RAJA-TODO: call cs.scorer.Update()
	cs.lock.Lock()
	defer cs.lock.Unlock()

	// Initial interval will have partial data
	if cs.lastUpdate.IsZero() {
		cs.lastUpdate = time.Now()
		cs.score = MaxScore
		return cs.score
	}

	cs.lastUpdate = time.Now()

	var params TrackScoreParams
	switch {
	case cs.trackInfo.Type == livekit.TrackType_AUDIO:
		_, maxAvailableLayerStats := getMaxAvailableLayerStats(streams, 0)
		if maxAvailableLayerStats == nil {
			// retain old score as stats will not be available when muted
			break
		}

		params = getTrackScoreParams(cs.codecName, maxAvailableLayerStats)
		packetRate := float64(params.PacketsExpected) / maxAvailableLayerStats.Duration.Seconds()
		if packetRate < audioPacketRateThreshold {
			// With DTX, it is possible to have fewer packets per second.
			// A loss with reduced packet rate has amplified negative effect on quality.
			// Opus uses 20 ms packetisation (50 pps). Calculate score only if packet rate is at least half of that.
			break
		}

		cs.score = AudioTrackScore(params)

	case cs.trackInfo.Type == livekit.TrackType_VIDEO:
		//
		// See note below about muxed tracks quality calculation challenges.
		//
		// This part concerns simulcast + dynacast challeges.
		// A sub-optimal solution is to measure only in windows where the max layer is stable.
		// With adaptive stream, it is possible that subscription changes max layer.
		// When expected layer changes from low -> high, the stats in that window
		// (when the change happens) will correspond to lower layer at least partially.
		// Using that to calculate against expected higher layer could result in lower score.
		//
		maxExpectedLayer := cs.params.GetMaxExpectedLayer()
		if maxExpectedLayer == buffer.InvalidLayerSpatial || maxExpectedLayer != cs.maxExpectedLayer {
			cs.maxExpectedLayer = maxExpectedLayer
			if maxExpectedLayer == buffer.InvalidLayerSpatial {
				// if not expecting data, reset the score to maximum
				cs.score = MaxScore
			}
			break
		}

		cs.maxExpectedLayer = maxExpectedLayer

		maxAvailableLayer, maxAvailableLayerStats := getMaxAvailableLayerStats(streams, maxExpectedLayer)
		if maxAvailableLayerStats == nil {
			// retain old score as stats will not be available when muted
			break
		}

		params = getTrackScoreParams(cs.codecName, maxAvailableLayerStats)

		// for muxed tracks, i. e. simulcast publisher muxed into a single track,
		// use the current spatial layer.
		// NOTE: This is still not perfect as muxing means layers could have changed
		// in the analysis window. Needs a more complex design to keep track of all layer
		// switches to be able to do precise calculations. As the window is small,
		// using the current and maximum is a reasonable approximation.
		if cs.params.GetCurrentLayerSpatial != nil {
			maxAvailableLayer = cs.params.GetCurrentLayerSpatial()
			if maxAvailableLayer == buffer.InvalidLayerSpatial {
				// retain old score as stats will not be available if not forwarding
				break
			}
		}
		params.Width, params.Height = cs.getLayerDimensions(maxAvailableLayer)
		if cs.trackInfo.Source == livekit.TrackSource_SCREEN_SHARE || params.Width == 0 || params.Height == 0 {
			if cs.params.GetIsReducedQuality != nil {
				_, params.IsReducedQuality = cs.params.GetIsReducedQuality()
			}

			cs.score = LossBasedTrackScore(params)
		} else {
			cs.score = VideoTrackScore(params)
			if cs.params.GetIsReducedQuality != nil {
				// penalty of one level per layer away from desired/expected layer
				distanceToDesired, isDeficient := cs.params.GetIsReducedQuality()
				if isDeficient {
					cs.score -= float32(distanceToDesired)
				}
				cs.score = float32(clamp(float64(cs.score), float64(MinScore), float64(MaxScore)))
			}
		}
	}

	return cs.score

	// RAJA-TODO - calculate max expected layer properly
	_, maxAvailableLayerStats := getMaxAvailableLayerStats(streams, 0)
	if maxAvailableLayerStats != nil {
		cs.scorer.Update(&windowStat{
			startTime:       maxAvailableLayerStats.StartTime,
			duration:        maxAvailableLayerStats.Duration,
			packetsExpected: maxAvailableLayerStats.Packets + maxAvailableLayerStats.PacketsPadding,
			packetsLost:     maxAvailableLayerStats.PacketsLost,
			rttMax:          maxAvailableLayerStats.RttMax,
			jitterMax:       maxAvailableLayerStats.JitterMax,
		})
	}

	return cs.scorer.GetMOS()
}

func (cs *ConnectionStats) getStat() *livekit.AnalyticsStat {
	if cs.params.GetDeltaStats == nil {
		return nil
	}

	streams := cs.params.GetDeltaStats()
	if len(streams) == 0 {
		return nil
	}

	analyticsStreams := make([]*livekit.AnalyticsStream, 0, len(streams))
	for ssrc, stream := range streams {
		as := toAnalyticsStream(ssrc, stream.RTPStats)

		//
		// add video layer if either
		//   1. Simulcast - even if there is only one layer per stream as it provides layer id
		//   2. A stream has multiple layers
		//
		if cs.trackInfo.Type == livekit.TrackType_VIDEO && (len(streams) > 1 || len(stream.Layers) > 1) {
			for layer, layerStats := range stream.Layers {
				as.VideoLayers = append(as.VideoLayers, toAnalyticsVideoLayer(layer, layerStats))
			}
		}

		analyticsStreams = append(analyticsStreams, as)
	}

	score := cs.updateScore(streams)

	return &livekit.AnalyticsStat{
		Score:   score,
		Streams: analyticsStreams,
		Mime:    cs.params.MimeType,
	}
}

func (cs *ConnectionStats) updateStatsWorker() {
	interval := cs.params.UpdateInterval
	if interval == 0 {
		interval = UpdateInterval
	}

	tk := time.NewTicker(interval)
	defer tk.Stop()

	/* RAJA-REMOVE
	for {
		select {
		case <-cs.done:
			return

		case <-tk.C:
			stat := cs.getStat()
			if stat == nil {
				continue
			}

			if cs.onStatsUpdate != nil {
				cs.onStatsUpdate(cs, stat)
			}
		}
	}
	*/

	for {
		<-tk.C

		if cs.isClosed.Load() {
			return
		}

		stat := cs.getStat()
		if stat == nil {
			continue
		}

		if cs.onStatsUpdate != nil {
			cs.onStatsUpdate(cs, stat)
		}
	}
}

// -----------------------------------------------------------------------

func getCodecNameFromMime(mime string) string {
	codecName := ""
	codecParsed := strings.Split(strings.ToLower(mime), "/")
	if len(codecParsed) > 1 {
		codecName = codecParsed[1]
	}
	return codecName
}

// how much weight to give to packet loss rate when calculating score.
// It is codec dependent.
// For audio:
//   o Opus without FEC or RED suffers the most through packet loss, hence has the highest weight
//   o RED with two packet redundancy can absorb two out of every three packets lost, so packet loss is not as detrimental and therefore lower weight
//
// For video:
//   o No in-built codec repair available, hence same for all codecs
func getPacketLossWeight(mimeType string, isFecEnabled bool) float64 {
	plw := float64(0.0)
	switch {
	case strings.EqualFold(mimeType, webrtc.MimeTypeOpus):
		plw = 8.0
		if isFecEnabled {
			plw /= 2.0
		}

	case strings.EqualFold(mimeType, "audio/red"):
		plw = 2.0
		if isFecEnabled {
			plw /= 2.0
		}

	case strings.HasPrefix(strings.ToLower(mimeType), "video/"):
		plw = 10.0
	}

	return plw
}

func toAnalyticsStream(ssrc uint32, deltaStats *buffer.RTPDeltaInfo) *livekit.AnalyticsStream {
	return &livekit.AnalyticsStream{
		Ssrc:              ssrc,
		PrimaryPackets:    deltaStats.Packets,
		PrimaryBytes:      deltaStats.Bytes,
		RetransmitPackets: deltaStats.PacketsDuplicate,
		RetransmitBytes:   deltaStats.BytesDuplicate,
		PaddingPackets:    deltaStats.PacketsPadding,
		PaddingBytes:      deltaStats.BytesPadding,
		PacketsLost:       deltaStats.PacketsLost,
		Frames:            deltaStats.Frames,
		Rtt:               deltaStats.RttMax,
		Jitter:            uint32(deltaStats.JitterMax),
		Nacks:             deltaStats.Nacks,
		Plis:              deltaStats.Plis,
		Firs:              deltaStats.Firs,
	}
}

func toAnalyticsVideoLayer(layer int32, layerStats *buffer.RTPDeltaInfo) *livekit.AnalyticsVideoLayer {
	return &livekit.AnalyticsVideoLayer{
		Layer:   layer,
		Packets: layerStats.Packets + layerStats.PacketsDuplicate + layerStats.PacketsPadding,
		Bytes:   layerStats.Bytes + layerStats.BytesDuplicate + layerStats.BytesPadding,
		Frames:  layerStats.Frames,
	}
}

func getMaxAvailableLayerStats(streams map[uint32]*buffer.StreamStatsWithLayers, maxExpectedLayer int32) (int32, *buffer.RTPDeltaInfo) {
	if maxExpectedLayer == buffer.InvalidLayerSpatial {
		return buffer.InvalidLayerSpatial, nil
	}

	maxAvailableLayer := buffer.InvalidLayerSpatial
	var maxAvailableLayerStats *buffer.RTPDeltaInfo
	for _, stream := range streams {
		for layer, layerStats := range stream.Layers {
			if int32(layer) > maxExpectedLayer {
				continue
			}

			if int32(layer) > maxAvailableLayer {
				maxAvailableLayer = int32(layer)
				maxAvailableLayerStats = layerStats
			}
		}
	}

	return maxAvailableLayer, maxAvailableLayerStats
}

// RAJA-REMOVE
func getTrackScoreParams(codec string, layerStats *buffer.RTPDeltaInfo) TrackScoreParams {
	return TrackScoreParams{
		Duration:        layerStats.Duration,
		Codec:           codec,
		PacketsExpected: layerStats.Packets + layerStats.PacketsPadding,
		PacketsLost:     layerStats.PacketsLost,
		Bytes:           layerStats.Bytes - layerStats.HeaderBytes, // only use media payload size
		Frames:          layerStats.Frames,
		Jitter:          layerStats.JitterMax,
		Rtt:             layerStats.RttMax,
	}
}

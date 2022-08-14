package connectionquality

import (
	"strings"
	"sync"
	"time"

	"go.uber.org/atomic"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/utils"
)

const (
	UpdateInterval           = 5 * time.Second
	audioPacketRateThreshold = float64(25.0)
)

type ConnectionStatsParams struct {
	UpdateInterval time.Duration
	/* RAJA-REMOVE
	CodecType              webrtc.RTPCodecType
	CodecName              string
	*/
	MimeType               string
	GetDeltaStats          func() map[uint32]*buffer.StreamStatsWithLayers
	IsDtxDisabled          func() bool
	GetLayerDimension      func(int32) (uint32, uint32)
	GetMaxExpectedLayer    func() (int32, uint32, uint32)
	GetCurrentLayerSpatial func() int32
	GetDistanceToDesired   func() int32
	Logger                 logger.Logger
}

type ConnectionStats struct {
	params ConnectionStatsParams
	// RAJA-REMOVE trackSource livekit.TrackSource
	codecName   string
	trackInfo   *livekit.TrackInfo
	normFactors [buffer.DefaultMaxLayerSpatial + 1]float32

	onStatsUpdate func(cs *ConnectionStats, stat *livekit.AnalyticsStat)

	lock             sync.RWMutex
	score            float32
	lastUpdate       time.Time
	isLowQuality     atomic.Bool
	maxExpectedLayer int32

	done     chan struct{}
	isClosed atomic.Bool
}

func NewConnectionStats(params ConnectionStatsParams) *ConnectionStats {
	return &ConnectionStats{
		params:    params,
		codecName: getCodecNameFromMime(params.MimeType), // LK-TODO: have to notify on codec change
		// RAJA-REMOVE trackSource:      livekit.TrackSource_UNKNOWN,
		score:            5.0,
		maxExpectedLayer: buffer.InvalidLayerSpatial,
		done:             make(chan struct{}),
	}
}

func (cs *ConnectionStats) Start(trackInfo *livekit.TrackInfo) {
	cs.lock.Lock()
	cs.trackInfo = trackInfo

	//
	// get best score and calculate normalization factor.
	// Raitonale: MOS modeling will yield a max score for a specific codec.
	// That is outside the control of an SFU. SFU can impair quality due
	// network issues. So, get the maximum for given codec and normalize
	// it to the highest score. So, under perfect conditions, it will
	// yield a MOS of 5 which is the highest rating. Any SFU induced impairment
	// or network impairment will result in a lower score and that is
	// the quality that is under SFU/infrastructure control.
	//
	if trackInfo.Type == livekit.TrackType_AUDIO {
		params := TrackScoreParams{
			Duration: time.Second,
			Codec:    cs.codecName,
			Bytes:    20000 / 8,
		}
		cs.normFactors[0] = 5.0 / AudioTrackScore(params)
	} else {
		for _, layer := range cs.trackInfo.Layers {
			spatial := utils.SpatialLayerForQuality(layer.Quality)
			// LK-TODO: would be good to have this in Trackinfo
			frameRate := uint32(30)
			switch spatial {
			case 0:
				frameRate = 15
			case 1:
				frameRate = 20
			}
			params := TrackScoreParams{
				Duration: time.Second,
				Codec:    cs.codecName,
				Bytes:    uint64(layer.Bitrate) / 8,
				Width:    layer.Width,
				Height:   layer.Height,
				Frames:   frameRate,
			}
			cs.params.Logger.Debugw("RAJA norm factors", "layer", utils.SpatialLayerForQuality(layer.Quality), "score", VideoTrackScore(params), "params", params) // REMOVE
			cs.normFactors[utils.SpatialLayerForQuality(layer.Quality)] = 5.0 / VideoTrackScore(params)
		}
	}
	cs.params.Logger.Debugw("RAJA norm factors", "factors", cs.normFactors) // REMOVE
	cs.lock.Unlock()

	go cs.updateStatsWorker()
}

func (cs *ConnectionStats) Close() {
	if cs.isClosed.Swap(true) {
		return
	}

	close(cs.done)
}

/* RAJA-REMOVE
func (cs *ConnectionStats) SetTrackSource(trackSource livekit.TrackSource) {
	cs.trackSource = trackSource
}
*/

func (cs *ConnectionStats) OnStatsUpdate(fn func(cs *ConnectionStats, stat *livekit.AnalyticsStat)) {
	cs.onStatsUpdate = fn
}

func (cs *ConnectionStats) GetScore() float32 {
	cs.lock.RLock()
	defer cs.lock.RUnlock()

	return cs.score
}

func (cs *ConnectionStats) getMaxAvailableLayerStats(streams map[uint32]*buffer.StreamStatsWithLayers, maxExpectedLayer int32) (int32, *buffer.RTPDeltaInfo) {
	maxAvailableLayer := buffer.InvalidLayerSpatial
	var maxAvailableLayerStats *buffer.RTPDeltaInfo
	for _, stream := range streams {
		for layer, layerStats := range stream.Layers {
			if int32(layer) > maxAvailableLayer {
				if maxExpectedLayer == buffer.InvalidLayerSpatial || int32(layer) <= maxExpectedLayer {
					maxAvailableLayer = int32(layer)
					maxAvailableLayerStats = layerStats
				}
			}
		}
	}

	return maxAvailableLayer, maxAvailableLayerStats
}

func (cs *ConnectionStats) updateScore(streams map[uint32]*buffer.StreamStatsWithLayers) float32 {
	cs.lock.Lock()
	defer cs.lock.Unlock()

	// Initial interval will have partial data
	if cs.lastUpdate.IsZero() {
		cs.lastUpdate = time.Now()
		cs.score = 5
		return cs.score
	}

	cs.lastUpdate = time.Now()

	switch {
	case cs.trackInfo.Type == livekit.TrackType_AUDIO:
		_, maxAvailableLayerStats := cs.getMaxAvailableLayerStats(streams, buffer.InvalidLayerSpatial)
		if maxAvailableLayerStats == nil {
			// retain old score as stats will not be available when muted
			return cs.score
		}

		params := getTrackScoreParams(cs.codecName, maxAvailableLayerStats)
		packetRate := float64(params.PacketsExpected) / maxAvailableLayerStats.Duration.Seconds()
		if packetRate < audioPacketRateThreshold {
			// With DTX, it is possible to have fewer packets per second.
			// A loss with reduced packet rate has amplified negative effect on quality.
			// Opus uses 20 ms packetisation (50 pps). Calculate score only if packet rate is at least half of that.
			return cs.score
		}

		cs.score = AudioTrackScore(params)

	case cs.trackInfo.Type == livekit.TrackType_VIDEO:
		// See note below about muxed tracks quality calculation challenges.
		// A sub-optimal solution is to measure only in windows where the max layer is stable.
		// With adaptive stream, it is possible that subscription changes max layer.
		// When expected layer changes from low -> high, the stats in that window
		// (when the change happens) will correspond to lower layer at least partially.
		// Using that to calculate against expected higher layer could result in lower score.
		maxExpectedLayer, expectedWidth, expectedHeight := cs.params.GetMaxExpectedLayer()
		if maxExpectedLayer == buffer.InvalidLayerSpatial || expectedWidth == 0 || expectedHeight == 0 || maxExpectedLayer != cs.maxExpectedLayer {
			cs.maxExpectedLayer = maxExpectedLayer
			cs.params.Logger.Debugw("return 5", "maxExpectedLayer", maxExpectedLayer, "width", expectedWidth, "height", expectedHeight, "existingMax", cs.maxExpectedLayer) // REMOVE
			return cs.score
		}

		cs.maxExpectedLayer = maxExpectedLayer

		maxAvailableLayer, maxAvailableLayerStats := cs.getMaxAvailableLayerStats(streams, maxExpectedLayer)
		if maxAvailableLayerStats == nil {
			// retain old score as stats will not be available when muted
			return cs.score
		}

		params := getTrackScoreParams(cs.codecName, maxAvailableLayerStats)

		// for muxed tracks, i. e. simulcast publisher muxed into a single track,
		// use the current spatial layer.
		// NOTE: This is still not perfect as muxing means layers could have changed
		// in the analysis window. Needs a more complex design to keep track of all layer
		// switches to be able to do precise calculations. As the window is small,
		// using the current and maximum is a reasonable approximation.
		if cs.params.GetCurrentLayerSpatial != nil {
			maxAvailableLayer = cs.params.GetCurrentLayerSpatial()
		}
		/* RAJA-REMOVE
		params.ActualWidth, params.ActualHeight = cs.params.GetLayerDimension(maxAvailableLayer)

		params.ExpectedWidth = expectedWidth
		params.ExpectedHeight = expectedHeight
		*/
		params.Width, params.Height = cs.params.GetLayerDimension(maxAvailableLayer)
		/* RAJA-TODO
		if cs.trackInfo.Source == livekit.TrackSource_SCREEN_SHARE {
			if cs.params.GetDistanceToDesired != nil {
				params.IsReducedQuality = cs.params.GetDistanceToDesired() > 0
			}

			cs.score = ScreenshareTrackScore(params)
		} else {
		*/
		cs.score = VideoTrackScore(params)
		if int(maxAvailableLayer) < len(cs.normFactors) {
			cs.score *= cs.normFactors[maxAvailableLayer]
		}
		if cs.score > 5 {
			cs.score = 5
		}
		if cs.params.GetDistanceToDesired != nil {
			cs.score -= float32(cs.params.GetDistanceToDesired())
			if cs.score < 1 {
				cs.score = 1
			}
		}
		/*
			}
		*/
		cs.params.Logger.Debugw("video connection quality score", "score", cs.score, "params", params)

		if cs.score < 3.5 {
			if !cs.isLowQuality.Swap(true) {
				// changed from good to low quality, log
				cs.params.Logger.Debugw("low connection quality", "score", cs.score, "params", params)
			}
		} else {
			cs.isLowQuality.Store(false)
		}
	}

	return cs.score
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

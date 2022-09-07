package connectionquality

import (
	"strings"
	"sync"
	"time"

	"go.uber.org/atomic"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
)

const (
	UpdateInterval           = 5 * time.Second
	audioPacketRateThreshold = float64(25.0)
)

type ConnectionStatsParams struct {
	UpdateInterval         time.Duration
	MimeType               string
	GetDeltaStats          func() map[uint32]*buffer.StreamStatsWithLayers
	GetMaxExpectedLayer    func() int32
	GetCurrentLayerSpatial func() int32
	GetIsReducedQuality    func() (int32, bool)
	Logger                 logger.Logger
}

type ConnectionStats struct {
	params      ConnectionStatsParams
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
		params:           params,
		codecName:        getCodecNameFromMime(params.MimeType), // LK-TODO: have to notify on codec change
		normFactors:      [buffer.DefaultMaxLayerSpatial + 1]float32{1, 1, 1},
		score:            MaxScore,
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
		// LK-TODO: would be good to have audio expected bitrate in Trackinfo
		params := TrackScoreParams{
			Duration: time.Second,
			Codec:    cs.codecName,
			Bytes:    20000 / 8,
		}
		cs.normFactors[0] = MaxScore / AudioTrackScore(params, 1)
	} else {
		for _, layer := range cs.trackInfo.Layers {
			spatial := buffer.VideoQualityToSpatialLayer(layer.Quality, cs.trackInfo)
			// LK-TODO: would be good to have expected frame rate in Trackinfo
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
			cs.normFactors[spatial] = MaxScore / VideoTrackScore(params, 1)
		}
	}
	cs.lock.Unlock()

	go cs.updateStatsWorker()
}

func (cs *ConnectionStats) Close() {
	if cs.isClosed.Swap(true) {
		return
	}

	close(cs.done)
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

func (cs *ConnectionStats) updateScore(streams map[uint32]*buffer.StreamStatsWithLayers) float32 {
	cs.lock.Lock()
	defer cs.lock.Unlock()

	// Initial interval will have partial data
	if cs.lastUpdate.IsZero() {
		cs.lastUpdate = time.Now()
		cs.score = MaxScore
		return cs.score
	}

	cs.lastUpdate = time.Now()

	switch {
	case cs.trackInfo.Type == livekit.TrackType_AUDIO:
		maxAvailableLayer, maxAvailableLayerStats := getMaxAvailableLayerStats(streams, 0)
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

		normFactor := float32(1)
		if int(maxAvailableLayer) < len(cs.normFactors) {
			normFactor = cs.normFactors[maxAvailableLayer]
		}
		cs.score = AudioTrackScore(params, normFactor)

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
			return cs.score
		}

		cs.maxExpectedLayer = maxExpectedLayer

		maxAvailableLayer, maxAvailableLayerStats := getMaxAvailableLayerStats(streams, maxExpectedLayer)
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
			if maxAvailableLayer == buffer.InvalidLayerSpatial {
				// retain old score as stats will not be available if not forwarding
				return cs.score
			}
		}
		params.Width, params.Height = cs.getLayerDimensions(maxAvailableLayer)
		if cs.trackInfo.Source == livekit.TrackSource_SCREEN_SHARE || params.Width == 0 || params.Height == 0 {
			if cs.params.GetIsReducedQuality != nil {
				_, params.IsReducedQuality = cs.params.GetIsReducedQuality()
			}

			cs.score = LossBasedTrackScore(params)
		} else {
			normFactor := float32(1)
			if int(maxAvailableLayer) < len(cs.normFactors) {
				normFactor = cs.normFactors[maxAvailableLayer]
			}
			cs.score = VideoTrackScore(params, normFactor)
			if cs.params.GetIsReducedQuality != nil {
				// penalty of one level per layer away from desired/expected layer
				distanceToDesired, isDeficient := cs.params.GetIsReducedQuality()
				if isDeficient {
					cs.score -= float32(distanceToDesired)
				}
				cs.score = float32(clamp(float64(cs.score), float64(MinScore), float64(MaxScore)))
			}
		}

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

func getMaxAvailableLayerStats(streams map[uint32]*buffer.StreamStatsWithLayers, maxExpectedLayer int32) (int32, *buffer.RTPDeltaInfo) {
	maxAvailableLayer := buffer.InvalidLayerSpatial
	var maxAvailableLayerStats *buffer.RTPDeltaInfo
	for _, stream := range streams {
		for layer, layerStats := range stream.Layers {
			if maxExpectedLayer == buffer.InvalidLayerSpatial || int32(layer) > maxExpectedLayer {
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

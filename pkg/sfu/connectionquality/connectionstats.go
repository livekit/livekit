package connectionquality

import (
	"sync"
	"time"

	"github.com/pion/webrtc/v3"
	"go.uber.org/atomic"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/utils"
)

const (
	UpdateInterval = 2 * time.Second
)

type ConnectionStatsParams struct {
	UpdateInterval         time.Duration
	CodecType              webrtc.RTPCodecType
	CodecName              string
	MimeType               string
	GetDeltaStats          func() map[uint32]*buffer.StreamStatsWithLayers
	IsDtxDisabled          func() bool
	GetLayerDimension      func(int32) (uint32, uint32)
	GetMaxExpectedLayer    func() *livekit.VideoLayer
	GetCurrentLayerSpatial func() int32
	GetIsReducedQuality    func() bool
	Logger                 logger.Logger
}

type ConnectionStats struct {
	params      ConnectionStatsParams
	trackSource livekit.TrackSource

	onStatsUpdate func(cs *ConnectionStats, stat *livekit.AnalyticsStat)

	lock       sync.RWMutex
	score      float32
	lastUpdate time.Time

	done     chan struct{}
	isClosed atomic.Bool
}

func NewConnectionStats(params ConnectionStatsParams) *ConnectionStats {
	return &ConnectionStats{
		params:      params,
		trackSource: livekit.TrackSource_UNKNOWN,
		score:       5.0,
		done:        make(chan struct{}),
	}
}

func (cs *ConnectionStats) Start() {
	go cs.updateStatsWorker()
}

func (cs *ConnectionStats) Close() {
	if cs.isClosed.Swap(true) {
		return
	}

	close(cs.done)
}

func (cs *ConnectionStats) SetTrackSource(trackSource livekit.TrackSource) {
	cs.trackSource = trackSource
}

func (cs *ConnectionStats) OnStatsUpdate(fn func(cs *ConnectionStats, stat *livekit.AnalyticsStat)) {
	cs.onStatsUpdate = fn
}

func (cs *ConnectionStats) GetScore() float32 {
	cs.lock.RLock()
	defer cs.lock.RUnlock()

	return cs.score
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
	maxAvailableLayer, maxAvailableLayerStats := cs.getMaxAvailableLayerStats(streams)
	if maxAvailableLayerStats == nil {
		// retain old score as stats will not be available when muted
		return cs.score
	}

	params := TrackScoreParams{
		Duration:        maxAvailableLayerStats.Duration,
		Codec:           cs.params.CodecName,
		PacketsExpected: maxAvailableLayerStats.Packets + maxAvailableLayerStats.PacketsPadding,
		PacketsLost:     maxAvailableLayerStats.PacketsLost,
		Bytes:           maxAvailableLayerStats.Bytes,
		Frames:          maxAvailableLayerStats.Frames,
		Jitter:          maxAvailableLayerStats.JitterMax,
		Rtt:             maxAvailableLayerStats.RttMax,
	}

	switch {
	case cs.trackSource == livekit.TrackSource_SCREEN_SHARE:
		if cs.params.GetIsReducedQuality != nil {
			params.IsReducedQuality = cs.params.GetIsReducedQuality()
		}
		cs.score = ScreenshareTrackScore(params)
	case cs.params.CodecType == webrtc.RTPCodecTypeAudio:
		if cs.params.IsDtxDisabled != nil {
			params.DtxDisabled = cs.params.IsDtxDisabled()
		}
		cs.score = AudioTrackScore(params)

	case cs.params.CodecType == webrtc.RTPCodecTypeVideo:
		// get tracks expected max layer and dimensions
		maxExpectedLayer := cs.params.GetMaxExpectedLayer()
		if maxExpectedLayer == nil || utils.SpatialLayerForQuality(maxExpectedLayer.Quality) == buffer.InvalidLayerSpatial {
			return cs.score
		}

		if maxAvailableLayerStats != nil {
			// for muxed tracks, i. e. simulcast publisher muxed into a single track,
			// use the current spatial layer.
			// NOTE: This is still not perfect as muxing means layers could have changed
			// in the analysis window. Needs a more complex design to keep track of all layer
			// switches to be able to do precise calculations. As the window is small,
			// using the current and maximum is a reasonable approximation.
			if cs.params.GetCurrentLayerSpatial != nil {
				maxAvailableLayer = cs.params.GetCurrentLayerSpatial()
			}
			params.ActualWidth, params.ActualHeight = cs.params.GetLayerDimension(maxAvailableLayer)
		}

		params.ExpectedWidth = maxExpectedLayer.Width
		params.ExpectedHeight = maxExpectedLayer.Height
		cs.score = VideoTrackScore(params)
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
		as := ToAnalyticsStream(ssrc, stream.RTPStats)

		//
		// add video layer if either
		//   1. Simulcast - even if there is only one layer per stream as it provides layer id
		//   2. A stream has multiple layers
		//
		if cs.params.CodecType == webrtc.RTPCodecTypeVideo && (len(streams) > 1 || len(stream.Layers) > 1) {
			for layer, layerStats := range stream.Layers {
				as.VideoLayers = append(as.VideoLayers, ToAnalyticsVideoLayer(layer, layerStats))
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

func ToAnalyticsStream(ssrc uint32, deltaStats *buffer.RTPDeltaInfo) *livekit.AnalyticsStream {
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

func ToAnalyticsVideoLayer(layer int32, layerStats *buffer.RTPDeltaInfo) *livekit.AnalyticsVideoLayer {
	return &livekit.AnalyticsVideoLayer{
		Layer:   layer,
		Packets: layerStats.Packets + layerStats.PacketsDuplicate + layerStats.PacketsPadding,
		Bytes:   layerStats.Bytes + layerStats.BytesDuplicate + layerStats.BytesPadding,
		Frames:  layerStats.Frames,
	}
}

func (cs *ConnectionStats) getMaxAvailableLayerStats(streams map[uint32]*buffer.StreamStatsWithLayers) (int32, *buffer.RTPDeltaInfo) {
	maxAvailableLayer := buffer.InvalidLayerSpatial
	var maxAvailableLayerStats *buffer.RTPDeltaInfo
	for _, stream := range streams {
		for layer, layerStats := range stream.Layers {
			if int32(layer) > maxAvailableLayer {
				maxAvailableLayer = int32(layer)
				maxAvailableLayerStats = layerStats
			}
		}
	}

	return maxAvailableLayer, maxAvailableLayerStats
}

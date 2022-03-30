package connectionquality

import (
	"sync"
	"time"

	"github.com/pion/webrtc/v3"
	"go.uber.org/atomic"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
)

const (
	connectionQualityUpdateInterval = 5 * time.Second
)

type ConnectionStatsParams struct {
	UpdateInterval      time.Duration
	CodecType           webrtc.RTPCodecType
	GetTrackStats       func() map[uint32]*buffer.StreamStatsWithLayers
	GetQualityParams    func() *buffer.ConnectionQualityParams
	GetIsReducedQuality func() bool
	Logger              logger.Logger
}

type ConnectionStats struct {
	params ConnectionStatsParams

	onStatsUpdate func(cs *ConnectionStats, stat *livekit.AnalyticsStat)

	lock  sync.RWMutex
	score float32

	done     chan struct{}
	isClosed atomic.Bool
}

func NewConnectionStats(params ConnectionStatsParams) *ConnectionStats {
	return &ConnectionStats{
		params: params,
		score:  4.0,
		done:   make(chan struct{}),
	}
}

func (cs *ConnectionStats) Start() {
	go cs.updateStats()
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

func (cs *ConnectionStats) updateScore() float32 {
	cs.lock.Lock()
	defer cs.lock.Unlock()

	s := cs.params.GetQualityParams()
	if s == nil {
		return cs.score
	}

	if cs.params.CodecType == webrtc.RTPCodecTypeAudio {
		cs.score = AudioConnectionScore(s.LossPercentage, s.Rtt, s.Jitter)
	} else {
		isReducedQuality := false
		if cs.params.GetIsReducedQuality != nil {
			isReducedQuality = cs.params.GetIsReducedQuality()
		}
		cs.score = VideoConnectionScore(s.LossPercentage, isReducedQuality)
	}

	return cs.score
}

func (cs *ConnectionStats) getStat() *livekit.AnalyticsStat {
	if cs.params.GetTrackStats == nil {
		return nil
	}

	streams := cs.params.GetTrackStats()
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
				as.VideoLayers = append(as.VideoLayers, ToAnalyticsVideoLayer(layer, &layerStats))
			}
		}

		analyticsStreams = append(analyticsStreams, as)
	}

	score := cs.updateScore()

	return &livekit.AnalyticsStat{
		Score:   score,
		Streams: analyticsStreams,
	}
}

func (cs *ConnectionStats) updateStats() {
	interval := cs.params.UpdateInterval
	if interval == 0 {
		interval = connectionQualityUpdateInterval
	}
	tk := time.NewTicker(interval)
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

func ToAnalyticsStream(ssrc uint32, rtpStats *livekit.RTPStats) *livekit.AnalyticsStream {
	return &livekit.AnalyticsStream{
		Ssrc:                   ssrc,
		TotalPrimaryPackets:    rtpStats.Packets,
		TotalPrimaryBytes:      rtpStats.Bytes,
		TotalRetransmitPackets: rtpStats.PacketsDuplicate,
		TotalRetransmitBytes:   rtpStats.BytesDuplicate,
		TotalPaddingPackets:    rtpStats.PacketsPadding,
		TotalPaddingBytes:      rtpStats.BytesPadding,
		TotalPacketsLost:       rtpStats.PacketsLost,
		TotalFrames:            rtpStats.Frames,
		Rtt:                    rtpStats.RttMax,
		Jitter:                 uint32(rtpStats.JitterMax),
		TotalNacks:             rtpStats.Nacks,
		TotalPlis:              rtpStats.Plis,
		TotalFirs:              rtpStats.Firs,
	}
}

func ToAnalyticsVideoLayer(layer int, layerStats *buffer.LayerStats) *livekit.AnalyticsVideoLayer {
	return &livekit.AnalyticsVideoLayer{
		Layer:        int32(layer),
		TotalPackets: layerStats.TotalPackets,
		TotalBytes:   layerStats.TotalBytes,
		TotalFrames:  layerStats.TotalFrames,
	}
}

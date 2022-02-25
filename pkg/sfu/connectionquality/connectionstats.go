package connectionquality

import (
	"sync"
	"time"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/pion/webrtc/v3"
	"go.uber.org/atomic"
)

const (
	connectionQualityUpdateInterval = 5 * time.Second
)

type qualityWindow struct {
	startSeqNum      uint32
	endSeqNum        uint32
	startPacketsLost uint32
	endPacketsLost   uint32
	maxRTT           uint32
	maxJitter        uint32
}

type ConnectionStatsParams struct {
	UpdateInterval      time.Duration
	CodecType           webrtc.RTPCodecType
	ClockRate           uint32
	GetTrackStats       func() map[uint32]*buffer.StreamStatsWithLayers
	GetIsReducedQuality func() bool
	Logger              logger.Logger
}

type ConnectionStats struct {
	params ConnectionStatsParams

	onStatsUpdate func(cs *ConnectionStats, stat *livekit.AnalyticsStat)

	lock           sync.RWMutex
	score          float32
	qualityWindows map[uint32]*qualityWindow

	done     chan struct{}
	isClosed atomic.Bool
}

func NewConnectionStats(params ConnectionStatsParams) *ConnectionStats {
	return &ConnectionStats{
		params:         params,
		score:          4.0,
		qualityWindows: make(map[uint32]*qualityWindow),
		done:           make(chan struct{}),
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

func (cs *ConnectionStats) UpdateWindow(ssrc uint32, extHighestSeqNum uint32, packetsLost uint32, rtt uint32, jitter uint32) {
	if cs.isClosed.Load() {
		return
	}

	cs.lock.Lock()
	defer cs.lock.Unlock()

	qw := cs.qualityWindows[ssrc]
	if qw == nil {
		qw = &qualityWindow{}
		cs.qualityWindows[ssrc] = qw
	}

	if qw.startSeqNum == 0 {
		qw.startSeqNum = extHighestSeqNum
		qw.startPacketsLost = packetsLost
	}

	if extHighestSeqNum > qw.endSeqNum {
		qw.endSeqNum = extHighestSeqNum
		qw.endPacketsLost = packetsLost
	}

	if rtt > qw.maxRTT {
		qw.maxRTT = rtt
	}

	if jitter > qw.maxJitter {
		qw.maxJitter = jitter
	}
}

func (cs *ConnectionStats) updateScore() float32 {
	cs.lock.Lock()
	defer cs.lock.Unlock()

	expectedPacketsInInterval := uint32(0)
	lostPacketsInInterval := uint32(0)
	maxRTT := uint32(0)
	maxJitter := uint32(0)
	for _, qw := range cs.qualityWindows {
		expectedPacketsInInterval += qw.endSeqNum - qw.startSeqNum + 1
		lostPacketsInInterval += qw.endPacketsLost - qw.startPacketsLost
		if qw.maxRTT > maxRTT {
			maxRTT = qw.maxRTT
		}
		if qw.maxJitter > maxJitter {
			maxJitter = qw.maxJitter
		}

		qw.startSeqNum = qw.endSeqNum
		qw.startPacketsLost = qw.endPacketsLost
		qw.maxRTT = 0
		qw.maxJitter = 0
	}

	pctLoss := float32(0.0)
	if int32(lostPacketsInInterval) < 0 {
		lostPacketsInInterval = 0
	}
	if expectedPacketsInInterval > 0 {
		pctLoss = (float32(lostPacketsInInterval) / float32(expectedPacketsInInterval)) * 100.0
	}

	if cs.params.CodecType == webrtc.RTPCodecTypeAudio {
		// covert jitter (in media samples units) to milliseconds
		cs.score = AudioConnectionScore(pctLoss, maxRTT, float32(maxJitter)*1000.0/float32(cs.params.ClockRate))
	} else {
		isReducedQuality := false
		if cs.params.GetIsReducedQuality != nil {
			isReducedQuality = cs.params.GetIsReducedQuality()
		}
		cs.score = VideoConnectionScore(pctLoss, isReducedQuality)
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
		maxRTT := stream.StreamStats.RTT
		maxJitter := uint32(stream.StreamStats.Jitter)

		if qw := cs.qualityWindows[ssrc]; qw != nil {
			maxRTT = qw.maxRTT
			maxJitter = qw.maxJitter
		}

		as := ToAnalyticsStream(ssrc, &stream.StreamStats, maxRTT, maxJitter, cs.params.ClockRate)

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

func ToAnalyticsStream(ssrc uint32, streamStats *buffer.StreamStats, maxRTT uint32, maxJitter uint32, clockRate uint32) *livekit.AnalyticsStream {
	// convert jitter (from number of media samples to microseconds
	jitter := uint32((float32(maxJitter) * 1e6) / float32(clockRate))
	return &livekit.AnalyticsStream{
		Ssrc:                   ssrc,
		TotalPrimaryPackets:    streamStats.TotalPrimaryPackets,
		TotalPrimaryBytes:      streamStats.TotalPrimaryBytes,
		TotalRetransmitPackets: streamStats.TotalRetransmitPackets,
		TotalRetransmitBytes:   streamStats.TotalRetransmitBytes,
		TotalPaddingPackets:    streamStats.TotalPaddingPackets,
		TotalPaddingBytes:      streamStats.TotalPaddingBytes,
		TotalPacketsLost:       streamStats.TotalPacketsLost,
		TotalFrames:            streamStats.TotalFrames,
		Rtt:                    maxRTT,
		Jitter:                 jitter,
		TotalNacks:             streamStats.TotalNACKs,
		TotalPlis:              streamStats.TotalPLIs,
		TotalFirs:              streamStats.TotalFIRs,
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

package connectionquality

import (
	"strings"
	"sync"
	"time"

	"github.com/frostbyte73/core"
	"github.com/pion/webrtc/v3"
	"go.uber.org/atomic"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

const (
	UpdateInterval           = 5 * time.Second
	processThreshold         = 0.95
	noStatsTooLongMultiplier = 2
)

type ConnectionStatsParams struct {
	UpdateInterval    time.Duration
	MimeType          string
	IsFECEnabled      bool
	IsDependentRTT    bool
	IsDependentJitter bool
	GetDeltaStats     func() map[uint32]*buffer.StreamStatsWithLayers
	Logger            logger.Logger
}

type ConnectionStats struct {
	params ConnectionStatsParams

	isStarted atomic.Bool
	isVideo   atomic.Bool

	onStatsUpdate func(cs *ConnectionStats, stat *livekit.AnalyticsStat)

	lock           sync.RWMutex
	lastStatsAt    time.Time
	statsInProcess bool

	scorer *qualityScorer

	done core.Fuse
}

func NewConnectionStats(params ConnectionStatsParams) *ConnectionStats {
	return &ConnectionStats{
		params: params,
		scorer: newQualityScorer(qualityScorerParams{
			PacketLossWeight:  getPacketLossWeight(params.MimeType, params.IsFECEnabled), // LK-TODO: have to notify codec change?
			IsDependentRTT:    params.IsDependentRTT,
			IsDependentJitter: params.IsDependentJitter,
			Logger:            params.Logger,
		}),
		done: core.NewFuse(),
	}
}

func (cs *ConnectionStats) Start(trackInfo *livekit.TrackInfo, at time.Time) {
	if cs.isStarted.Swap(true) {
		return
	}

	cs.isVideo.Store(trackInfo.Type == livekit.TrackType_VIDEO)

	cs.updateLastStatsAt(time.Now()) // force an initial wait

	cs.scorer.Start(at)

	go cs.updateStatsWorker()
}

func (cs *ConnectionStats) Close() {
	cs.done.Break()
}

func (cs *ConnectionStats) OnStatsUpdate(fn func(cs *ConnectionStats, stat *livekit.AnalyticsStat)) {
	cs.onStatsUpdate = fn
}

func (cs *ConnectionStats) UpdateMute(isMuted bool, at time.Time) {
	cs.scorer.UpdateMute(isMuted, at)
}

func (cs *ConnectionStats) AddBitrateTransition(bitrate int64, at time.Time) {
	cs.scorer.AddBitrateTransition(bitrate, at)
}

func (cs *ConnectionStats) UpdateLayerMute(isMuted bool, at time.Time) {
	cs.scorer.UpdateLayerMute(isMuted, at)
}

func (cs *ConnectionStats) AddLayerTransition(distance float64, at time.Time) {
	cs.scorer.AddLayerTransition(distance, at)
}

func (cs *ConnectionStats) GetScoreAndQuality() (float32, livekit.ConnectionQuality) {
	return cs.scorer.GetMOSAndQuality()
}

func (cs *ConnectionStats) ReceiverReportReceived(at time.Time) {
	cs.getStat(at)
}

func (cs *ConnectionStats) updateScore(streams map[uint32]*buffer.StreamStatsWithLayers, at time.Time) float32 {
	deltaInfoList := make([]*buffer.RTPDeltaInfo, 0, len(streams))
	for _, s := range streams {
		deltaInfoList = append(deltaInfoList, s.RTPStats)
	}
	agg := buffer.AggregateRTPDeltaInfo(deltaInfoList)

	var stat windowStat
	if agg != nil {
		stat.startedAt = agg.StartTime
		stat.duration = agg.Duration
		stat.packetsExpected = agg.Packets + agg.PacketsPadding
		stat.packetsLost = agg.PacketsLost
		stat.packetsMissing = agg.PacketsMissing
		stat.bytes = agg.Bytes - agg.HeaderBytes // only use media payload size
		stat.rttMax = agg.RttMax
		stat.jitterMax = agg.JitterMax
	}
	cs.scorer.Update(&stat, at)

	mos, _ := cs.scorer.GetMOSAndQuality()
	return mos
}

func (cs *ConnectionStats) maybeMarkInProcess() bool {
	cs.lock.Lock()
	defer cs.lock.Unlock()

	if cs.statsInProcess {
		// already running
		return false
	}

	interval := cs.params.UpdateInterval
	if interval == 0 {
		interval = UpdateInterval
	}

	if cs.isStarted.Load() && time.Since(cs.lastStatsAt) > time.Duration(processThreshold*float64(interval)) {
		cs.statsInProcess = true
		return true
	}

	return false
}

func (cs *ConnectionStats) updateLastStatsAt(at time.Time) {
	cs.lock.Lock()
	defer cs.lock.Unlock()

	cs.lastStatsAt = at
}

func (cs *ConnectionStats) isTooLongSinceLastStats() bool {
	cs.lock.Lock()
	defer cs.lock.Unlock()

	interval := cs.params.UpdateInterval
	if interval == 0 {
		interval = UpdateInterval
	}
	return !cs.lastStatsAt.IsZero() && time.Since(cs.lastStatsAt) > interval*noStatsTooLongMultiplier
}

func (cs *ConnectionStats) clearInProcess() {
	cs.lock.Lock()
	defer cs.lock.Unlock()

	cs.statsInProcess = false
}

func (cs *ConnectionStats) getStat(at time.Time) {
	if cs.params.GetDeltaStats == nil {
		return
	}

	if !cs.maybeMarkInProcess() {
		// not yet time to process
		return
	}

	streams := cs.params.GetDeltaStats()
	if len(streams) == 0 {
		if cs.isTooLongSinceLastStats() {
			cs.updateLastStatsAt(at)
			cs.updateScore(streams, at)
		}
		cs.clearInProcess()
		return
	}

	// stats available, update last stats time
	cs.updateLastStatsAt(at)

	score := cs.updateScore(streams, at)

	if cs.onStatsUpdate != nil {
		analyticsStreams := make([]*livekit.AnalyticsStream, 0, len(streams))
		for ssrc, stream := range streams {
			as := toAnalyticsStream(ssrc, stream.RTPStats)

			//
			// add video layer if either
			//   1. Simulcast - even if there is only one layer per stream as it provides layer id
			//   2. A stream has multiple layers
			//
			if (len(streams) > 1 || len(stream.Layers) > 1) && cs.isVideo.Load() {
				for layer, layerStats := range stream.Layers {
					as.VideoLayers = append(as.VideoLayers, toAnalyticsVideoLayer(layer, layerStats))
				}
			}

			analyticsStreams = append(analyticsStreams, as)
		}

		cs.onStatsUpdate(cs, &livekit.AnalyticsStat{
			Score:   score,
			Streams: analyticsStreams,
			Mime:    cs.params.MimeType,
		})
	}

	cs.clearInProcess()
}

func (cs *ConnectionStats) updateStatsWorker() {
	interval := cs.params.UpdateInterval
	if interval == 0 {
		interval = UpdateInterval
	}

	tk := time.NewTicker(interval)
	defer tk.Stop()

	done := cs.done.Watch()
	for {
		select {
		case <-done:
			return

		case <-tk.C:
			if cs.done.IsBroken() {
				return
			}

			cs.getStat(time.Now())
		}
	}
}

// -----------------------------------------------------------------------

// how much weight to give to packet loss rate when calculating score.
// It is codec dependent.
// For audio:
//
//	o Opus without FEC or RED suffers the most through packet loss, hence has the highest weight
//	o RED with two packet redundancy can absorb two out of every three packets lost, so packet loss is not as detrimental and therefore lower weight
//
// For video:
//
//	o No in-built codec repair available, hence same for all codecs
func getPacketLossWeight(mimeType string, isFecEnabled bool) float64 {
	var plw float64
	switch {
	case strings.EqualFold(mimeType, webrtc.MimeTypeOpus):
		// 2.5%: fall to GOOD, 7.5%: fall to POOR
		plw = 8.0
		if isFecEnabled {
			// 3.75%: fall to GOOD, 11.25%: fall to POOR
			plw /= 1.5
		}

	case strings.EqualFold(mimeType, "audio/red"):
		// 6.66%: fall to GOOD, 20.0%: fall to POOR
		plw = 3.0
		if isFecEnabled {
			// 10%: fall to GOOD, 30.0%: fall to POOR
			plw /= 1.5
		}

	case strings.HasPrefix(strings.ToLower(mimeType), "video/"):
		// 2%: fall to GOOD, 6%: fall to POOR
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

package connectionquality

import (
	"strings"
	"time"

	"github.com/frostbyte73/core"

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
	UpdateInterval time.Duration
	MimeType       string
	IsFECEnabled   bool
	GetDeltaStats  func() map[uint32]*buffer.StreamStatsWithLayers
	Logger         logger.Logger
}

type ConnectionStats struct {
	params    ConnectionStatsParams
	trackInfo *livekit.TrackInfo

	onStatsUpdate func(cs *ConnectionStats, stat *livekit.AnalyticsStat)

	scorer *qualityScorer

	done core.Fuse
}

func NewConnectionStats(params ConnectionStatsParams) *ConnectionStats {
	return &ConnectionStats{
		params: params,
		scorer: newQualityScorer(qualityScorerParams{
			PacketLossWeight: getPacketLossWeight(params.MimeType, params.IsFECEnabled), // LK-TODO: have to notify codec change?
			Logger:           params.Logger,
		}),
		done: core.NewFuse(),
	}
}

func (cs *ConnectionStats) Start(trackInfo *livekit.TrackInfo, at time.Time) {
	cs.trackInfo = trackInfo

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

func (cs *ConnectionStats) AddTransition(bitrate int64, at time.Time) {
	cs.scorer.AddTransition(bitrate, at)
}

func (cs *ConnectionStats) GetScoreAndQuality() (float32, livekit.ConnectionQuality) {
	return cs.scorer.GetMOSAndQuality()
}

func (cs *ConnectionStats) updateScore(streams map[uint32]*buffer.StreamStatsWithLayers, at time.Time) float32 {
	if len(streams) == 0 {
		cs.scorer.Update(nil, at)
	} else {
		var stat windowStat
		for _, s := range streams {
			if stat.startedAt.IsZero() || stat.startedAt.After(s.RTPStats.StartTime) {
				stat.startedAt = s.RTPStats.StartTime
			}
			if stat.duration < s.RTPStats.Duration {
				stat.duration = s.RTPStats.Duration
			}
			stat.packetsExpected += s.RTPStats.Packets + s.RTPStats.PacketsPadding
			stat.packetsLost += s.RTPStats.PacketsLost
			stat.packetsMissing += s.RTPStats.PacketsMissing
			if stat.rttMax < s.RTPStats.RttMax {
				stat.rttMax = s.RTPStats.RttMax
			}
			if stat.jitterMax < s.RTPStats.JitterMax {
				stat.jitterMax = s.RTPStats.JitterMax
			}
			stat.bytes += s.RTPStats.Bytes - s.RTPStats.HeaderBytes // only use media payload size
		}
		cs.scorer.Update(&stat, at)
	}

	mos, _ := cs.scorer.GetMOSAndQuality()
	return mos
}

func (cs *ConnectionStats) getStat(at time.Time) *livekit.AnalyticsStat {
	if cs.params.GetDeltaStats == nil {
		return nil
	}

	streams := cs.params.GetDeltaStats()
	if len(streams) == 0 {
		cs.updateScore(streams, at)
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

	score := cs.updateScore(streams, at)

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
		case <-cs.done.Watch():
			return

		case <-tk.C:
			if cs.done.IsClosed() {
				return
			}

			stat := cs.getStat(time.Now())
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
		// 2.5%: fall to GOOD, 5%: fall to POOR
		plw = 8.0
		if isFecEnabled {
			// 3.75%: fall to GOOD, 7.5%: fall to POOR
			plw /= 1.5
		}

	case strings.EqualFold(mimeType, "audio/red"):
		// 6.66%: fall to GOOD, 13.33%: fall to POOR
		plw = 3.0
		if isFecEnabled {
			// 10%: fall to GOOD, 20%: fall to POOR
			plw /= 1.5
		}

	case strings.HasPrefix(strings.ToLower(mimeType), "video/"):
		// 2%: fall to GOOD, 4%: fall to POOR
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

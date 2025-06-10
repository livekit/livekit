// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package connectionquality

import (
	"sync"
	"time"

	"github.com/frostbyte73/core"
	"go.uber.org/atomic"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/mime"
	"github.com/livekit/livekit-server/pkg/sfu/rtpstats"
)

const (
	UpdateInterval                   = 5 * time.Second
	noReceiverReportTooLongThreshold = 30 * time.Second
)

type ConnectionStatsReceiverProvider interface {
	GetDeltaStats() map[uint32]*buffer.StreamStatsWithLayers
	GetLastSenderReportTime() time.Time
}

type ConnectionStatsSenderProvider interface {
	GetDeltaStatsSender() map[uint32]*buffer.StreamStatsWithLayers
	GetPrimaryStreamLastReceiverReportTime() time.Time
	GetPrimaryStreamPacketsSent() uint64
}

type ConnectionStatsParams struct {
	UpdateInterval     time.Duration
	IncludeRTT         bool
	IncludeJitter      bool
	EnableBitrateScore bool
	ReceiverProvider   ConnectionStatsReceiverProvider
	SenderProvider     ConnectionStatsSenderProvider
	Logger             logger.Logger
}

type ConnectionStats struct {
	params ConnectionStatsParams

	codecMimeType atomic.Value // mime.MimeType

	isStarted atomic.Bool
	isVideo   atomic.Bool

	onStatsUpdate func(cs *ConnectionStats, stat *livekit.AnalyticsStat)

	lock               sync.RWMutex
	packetsSent        uint64
	streamingStartedAt time.Time

	scorer *qualityScorer

	done core.Fuse
}

func NewConnectionStats(params ConnectionStatsParams) *ConnectionStats {
	return &ConnectionStats{
		params: params,
		scorer: newQualityScorer(qualityScorerParams{
			IncludeRTT:         params.IncludeRTT,
			IncludeJitter:      params.IncludeJitter,
			EnableBitrateScore: params.EnableBitrateScore,
			Logger:             params.Logger,
		}),
	}
}

func (cs *ConnectionStats) StartAt(codecMimeType mime.MimeType, isFECEnabled bool, at time.Time) {
	if cs.isStarted.Swap(true) {
		return
	}

	cs.isVideo.Store(mime.IsMimeTypeVideo(codecMimeType))
	cs.codecMimeType.Store(codecMimeType)
	cs.scorer.StartAt(getPacketLossWeight(codecMimeType, isFECEnabled), at)

	go cs.updateStatsWorker()
}

func (cs *ConnectionStats) Start(codecMimeType mime.MimeType, isFECEnabled bool) {
	cs.StartAt(codecMimeType, isFECEnabled, time.Now())
}

func (cs *ConnectionStats) Close() {
	cs.done.Break()
}

func (cs *ConnectionStats) UpdateCodec(codecMimeType mime.MimeType, isFECEnabled bool) {
	cs.isVideo.Store(mime.IsMimeTypeVideo(codecMimeType))
	cs.codecMimeType.Store(codecMimeType)
	cs.scorer.UpdatePacketLossWeight(getPacketLossWeight(codecMimeType, isFECEnabled))
}

func (cs *ConnectionStats) OnStatsUpdate(fn func(cs *ConnectionStats, stat *livekit.AnalyticsStat)) {
	cs.onStatsUpdate = fn
}

func (cs *ConnectionStats) UpdateMuteAt(isMuted bool, at time.Time) {
	if cs.done.IsBroken() {
		return
	}

	cs.scorer.UpdateMuteAt(isMuted, at)
}

func (cs *ConnectionStats) UpdateMute(isMuted bool) {
	if cs.done.IsBroken() {
		return
	}

	cs.scorer.UpdateMute(isMuted)
}

func (cs *ConnectionStats) AddBitrateTransitionAt(bitrate int64, at time.Time) {
	if cs.done.IsBroken() {
		return
	}

	cs.scorer.AddBitrateTransitionAt(bitrate, at)
}

func (cs *ConnectionStats) AddBitrateTransition(bitrate int64) {
	if cs.done.IsBroken() {
		return
	}

	cs.scorer.AddBitrateTransition(bitrate)
}

func (cs *ConnectionStats) UpdateLayerMuteAt(isMuted bool, at time.Time) {
	if cs.done.IsBroken() {
		return
	}

	cs.scorer.UpdateLayerMuteAt(isMuted, at)
}

func (cs *ConnectionStats) UpdateLayerMute(isMuted bool) {
	if cs.done.IsBroken() {
		return
	}

	cs.scorer.UpdateLayerMute(isMuted)
}

func (cs *ConnectionStats) UpdatePauseAt(isPaused bool, at time.Time) {
	if cs.done.IsBroken() {
		return
	}

	cs.scorer.UpdatePauseAt(isPaused, at)
}

func (cs *ConnectionStats) UpdatePause(isPaused bool) {
	if cs.done.IsBroken() {
		return
	}

	cs.scorer.UpdatePause(isPaused)
}

func (cs *ConnectionStats) AddLayerTransitionAt(distance float64, at time.Time) {
	if cs.done.IsBroken() {
		return
	}

	cs.scorer.AddLayerTransitionAt(distance, at)
}

func (cs *ConnectionStats) AddLayerTransition(distance float64) {
	if cs.done.IsBroken() {
		return
	}

	cs.scorer.AddLayerTransition(distance)
}

func (cs *ConnectionStats) GetScoreAndQuality() (float32, livekit.ConnectionQuality) {
	return cs.scorer.GetMOSAndQuality()
}

func (cs *ConnectionStats) updateScoreWithAggregate(agg *rtpstats.RTPDeltaInfo, lastRTCPAt time.Time, at time.Time) float32 {
	var stat windowStat
	if agg != nil {
		stat.startedAt = agg.StartTime
		stat.duration = agg.EndTime.Sub(agg.StartTime)
		stat.packets = agg.Packets
		stat.packetsPadding = agg.PacketsPadding
		stat.packetsLost = agg.PacketsLost
		stat.packetsMissing = agg.PacketsMissing
		stat.packetsOutOfOrder = agg.PacketsOutOfOrder
		stat.bytes = agg.Bytes - agg.HeaderBytes // only use media payload size
		stat.rttMax = agg.RttMax
		stat.jitterMax = agg.JitterMax

		stat.lastRTCPAt = lastRTCPAt
	}
	if at.IsZero() {
		cs.scorer.Update(&stat)
	} else {
		cs.scorer.UpdateAt(&stat, at)
	}

	mos, _ := cs.scorer.GetMOSAndQuality()
	return mos
}

func (cs *ConnectionStats) updateScoreFromReceiverReport(at time.Time) (float32, map[uint32]*buffer.StreamStatsWithLayers) {
	if cs.params.SenderProvider == nil {
		return MinMOS, nil
	}

	streamingStartedAt := cs.updateStreamingStart(at)
	if streamingStartedAt.IsZero() {
		// not streaming, just return current score
		mos, _ := cs.scorer.GetMOSAndQuality()
		return mos, nil
	}

	streams := cs.params.SenderProvider.GetDeltaStatsSender()
	if len(streams) == 0 {
		//  check for receiver report not received for a while
		marker := cs.params.SenderProvider.GetPrimaryStreamLastReceiverReportTime()
		if marker.IsZero() || streamingStartedAt.After(marker) {
			marker = streamingStartedAt
		}
		if time.Since(marker) > noReceiverReportTooLongThreshold {
			// have not received receiver report for a long time when streaming, run with nil stat
			return cs.updateScoreWithAggregate(nil, time.Time{}, at), nil
		}

		// wait for receiver report, return current score
		mos, _ := cs.scorer.GetMOSAndQuality()
		return mos, nil
	}

	// delta stat duration could be large due to not receiving receiver report for a long time (for example, due to mute),
	// adjust to streaming start if necessary
	if streamingStartedAt.After(cs.params.SenderProvider.GetPrimaryStreamLastReceiverReportTime()) {
		// last receiver report was before streaming started, wait for next one
		mos, _ := cs.scorer.GetMOSAndQuality()
		return mos, streams
	}

	agg := toAggregateDeltaInfo(streams, true)
	if agg == nil {
		// no receiver report in the window
		mos, _ := cs.scorer.GetMOSAndQuality()
		return mos, streams
	}
	if streamingStartedAt.After(agg.StartTime) {
		agg.StartTime = streamingStartedAt
	}
	return cs.updateScoreWithAggregate(agg, time.Time{}, at), streams
}

func (cs *ConnectionStats) updateScoreAt(at time.Time) (float32, map[uint32]*buffer.StreamStatsWithLayers, bool) {
	if cs.params.SenderProvider != nil {
		// receiver report based quality scoring, use stats from receiver report for scoring
		score, streams := cs.updateScoreFromReceiverReport(at)
		return score, streams, true
	}

	if cs.params.ReceiverProvider == nil {
		return MinMOS, nil, false
	}

	streams := cs.params.ReceiverProvider.GetDeltaStats()
	if len(streams) == 0 {
		mos, _ := cs.scorer.GetMOSAndQuality()
		return mos, nil, false
	}

	agg := toAggregateDeltaInfo(streams, false)
	if agg == nil {
		// no receiver report in the window
		mos, _ := cs.scorer.GetMOSAndQuality()
		return mos, streams, false
	}
	return cs.updateScoreWithAggregate(agg, cs.params.ReceiverProvider.GetLastSenderReportTime(), at), streams, false
}

func (cs *ConnectionStats) updateStreamingStart(at time.Time) time.Time {
	cs.lock.Lock()
	defer cs.lock.Unlock()

	packetsSent := cs.params.SenderProvider.GetPrimaryStreamPacketsSent()
	if packetsSent > cs.packetsSent {
		if cs.streamingStartedAt.IsZero() {
			// the start could be anywhere after last update, but using `at` as this is not required to be accurate
			if at.IsZero() {
				cs.streamingStartedAt = time.Now()
			} else {
				cs.streamingStartedAt = at
			}
		}
	} else {
		cs.streamingStartedAt = time.Time{}
	}
	cs.packetsSent = packetsSent

	return cs.streamingStartedAt
}

func (cs *ConnectionStats) getStat() {
	score, streams, isSender := cs.updateScoreAt(time.Time{})

	if cs.onStatsUpdate != nil && len(streams) != 0 {
		analyticsStreams := make([]*livekit.AnalyticsStream, 0, len(streams))
		for ssrc, stream := range streams {
			as := toAnalyticsStream(ssrc, stream.RTPStats, stream.RTPStatsRemoteView, isSender)
			if as == nil {
				continue
			}

			//
			// add video layer if either
			//   1. Simulcast - even if there is only one layer per stream as it provides layer id
			//   2. A stream has multiple layers
			//
			if (len(streams) > 1 || len(stream.Layers) > 1) && cs.isVideo.Load() {
				for layer, layerStats := range stream.Layers {
					avl := toAnalyticsVideoLayer(layer, layerStats)
					if avl != nil {
						as.VideoLayers = append(as.VideoLayers, avl)
					}
				}
			}

			analyticsStreams = append(analyticsStreams, as)
		}

		if len(analyticsStreams) != 0 {
			cs.onStatsUpdate(cs, &livekit.AnalyticsStat{
				Score:   score,
				Streams: analyticsStreams,
				Mime:    cs.codecMimeType.Load().(mime.MimeType).String(),
			})
		}
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
			cs.getStat()
			return

		case <-tk.C:
			if cs.done.IsBroken() {
				return
			}

			cs.getStat()
		}
	}
}

// -----------------------------------------------------------------------

// how much weight to give to packet loss rate when calculating score.
// It is codec dependent.
// For audio:
//
//	o Opus without FEC or RED suffers the most through packet loss, hence has the highest weight
//	o RED with two packet redundancy can absorb one out of every two packets lost, so packet loss is not as detrimental and therefore lower weight
//
// For video:
//
//	o No in-built codec repair available, hence same for all codecs
func getPacketLossWeight(mimeType mime.MimeType, isFecEnabled bool) float64 {
	var plw float64
	switch {
	case mimeType == mime.MimeTypeOpus:
		// 2.5%: fall to GOOD, 7.5%: fall to POOR
		plw = 8.0
		if isFecEnabled {
			// 3.75%: fall to GOOD, 11.25%: fall to POOR
			plw /= 1.5
		}

	case mimeType == mime.MimeTypeRED:
		// 5%: fall to GOOD, 15.0%: fall to POOR
		plw = 4.0
		if isFecEnabled {
			// 7.5%: fall to GOOD, 22.5%: fall to POOR
			plw /= 1.5
		}

	case mime.IsMimeTypeVideo(mimeType):
		// 2%: fall to GOOD, 6%: fall to POOR
		plw = 10.0
	}

	return plw
}

func toAggregateDeltaInfo(streams map[uint32]*buffer.StreamStatsWithLayers, useRemoteView bool) *rtpstats.RTPDeltaInfo {
	deltaInfoList := make([]*rtpstats.RTPDeltaInfo, 0, len(streams))
	for _, s := range streams {
		if useRemoteView {
			if s.RTPStatsRemoteView != nil {
				// discount jitter from publisher side + internal processing while reporting downstream jitter
				if s.RTPStats != nil {
					s.RTPStatsRemoteView.JitterMax -= s.RTPStats.JitterMax
					if s.RTPStatsRemoteView.JitterMax < 0.0 {
						s.RTPStatsRemoteView.JitterMax = 0.0
					}
				}
				deltaInfoList = append(deltaInfoList, s.RTPStatsRemoteView)
			}
		} else {
			if s.RTPStats != nil {
				deltaInfoList = append(deltaInfoList, s.RTPStats)
			}
		}
	}
	return rtpstats.AggregateRTPDeltaInfo(deltaInfoList)
}

func toAnalyticsStream(
	ssrc uint32,
	deltaStats *rtpstats.RTPDeltaInfo,
	deltaStatsRemoteView *rtpstats.RTPDeltaInfo,
	isSender bool,
) *livekit.AnalyticsStream {
	if deltaStats == nil {
		return nil
	}

	// discount the feed side loss when reporting forwarded track stats,
	// discount jitter from publisher side + internal processing while reporting downstream jitter
	packetsLost := deltaStats.PacketsLost
	rtt := deltaStats.RttMax
	maxJitter := deltaStats.JitterMax
	if deltaStatsRemoteView != nil {
		packetsLost = deltaStatsRemoteView.PacketsLost
		if deltaStatsRemoteView.PacketsMissing > packetsLost {
			packetsLost = 0
		} else {
			packetsLost -= deltaStatsRemoteView.PacketsMissing
		}

		rtt = deltaStatsRemoteView.RttMax
		maxJitter = deltaStatsRemoteView.JitterMax
	} else if isSender {
		rtt = 0
		maxJitter = 0
	}

	return &livekit.AnalyticsStream{
		StartTime:         timestamppb.New(deltaStats.StartTime),
		EndTime:           timestamppb.New(deltaStats.EndTime),
		Ssrc:              ssrc,
		PrimaryPackets:    deltaStats.Packets,
		PrimaryBytes:      deltaStats.Bytes,
		RetransmitPackets: deltaStats.PacketsDuplicate,
		RetransmitBytes:   deltaStats.BytesDuplicate,
		PaddingPackets:    deltaStats.PacketsPadding,
		PaddingBytes:      deltaStats.BytesPadding,
		PacketsLost:       packetsLost,
		PacketsOutOfOrder: deltaStats.PacketsOutOfOrder,
		Frames:            deltaStats.Frames,
		Rtt:               rtt,
		Jitter:            uint32(maxJitter),
		Nacks:             deltaStats.Nacks,
		Plis:              deltaStats.Plis,
		Firs:              deltaStats.Firs,
	}
}

func toAnalyticsVideoLayer(layer int32, layerStats *rtpstats.RTPDeltaInfo) *livekit.AnalyticsVideoLayer {
	if layerStats == nil {
		return nil
	}

	avl := &livekit.AnalyticsVideoLayer{
		Layer:   layer,
		Packets: layerStats.Packets + layerStats.PacketsDuplicate + layerStats.PacketsPadding,
		Bytes:   layerStats.Bytes + layerStats.BytesDuplicate + layerStats.BytesPadding,
		Frames:  layerStats.Frames,
	}
	if avl.Packets == 0 || avl.Bytes == 0 || avl.Frames == 0 {
		return nil
	}

	return avl
}

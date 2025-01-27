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
	"strings"
	"sync"
	"time"

	"github.com/frostbyte73/core"
	"github.com/pion/webrtc/v4"
	"go.uber.org/atomic"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/rtpstats"
)

const (
	updateIntervalDefault            = 5 * time.Second
	noReceiverReportTooLongThreshold = 30 * time.Second
)

type ConnectionQualityReceiverProvider interface {
	GetConnectionQualityDeltaStats() map[uint32]*buffer.StreamStatsWithLayers
	GetLastSenderReportTime() time.Time
}

type ConnectionQualitySenderProvider interface {
	GetConnectionQualityDeltaStatsSender() map[uint32]*buffer.StreamStatsWithLayers
	GetPrimaryStreamLastReceiverReportTime() time.Time
	GetPrimaryStreamPacketsSent() uint64
}

type ConnectionQualityParams struct {
	UpdateInterval     time.Duration
	IncludeRTT         bool
	IncludeJitter      bool
	EnableBitrateScore bool
	ReceiverProvider   ConnectionQualityReceiverProvider
	SenderProvider     ConnectionQualitySenderProvider
	Logger             logger.Logger
}

type ConnectionQuality struct {
	params ConnectionQualityParams

	isStarted atomic.Bool

	lock               sync.RWMutex
	packetsSent        uint64
	streamingStartedAt time.Time

	scorer *qualityScorer

	done core.Fuse
}

func NewConnectionQuality(params ConnectionQualityParams) *ConnectionQuality {
	return &ConnectionQuality{
		params: params,
		scorer: newQualityScorer(qualityScorerParams{
			IncludeRTT:         params.IncludeRTT,
			IncludeJitter:      params.IncludeJitter,
			EnableBitrateScore: params.EnableBitrateScore,
			Logger:             params.Logger,
		}),
	}
}

func (cq *ConnectionQuality) StartAt(codecMimeType string, isFECEnabled bool, at time.Time) {
	if cq.isStarted.Swap(true) {
		return
	}

	cq.scorer.StartAt(getPacketLossWeight(codecMimeType, isFECEnabled), at)

	go cq.updateScoreWorker()
}

func (cq *ConnectionQuality) Start(codecMimeType string, isFECEnabled bool) {
	cq.StartAt(codecMimeType, isFECEnabled, time.Now())
}

func (cq *ConnectionQuality) Close() {
	cq.done.Break()
}

func (cq *ConnectionQuality) UpdateMuteAt(isMuted bool, at time.Time) {
	if cq.done.IsBroken() {
		return
	}

	cq.scorer.UpdateMuteAt(isMuted, at)
}

func (cq *ConnectionQuality) UpdateMute(isMuted bool) {
	if cq.done.IsBroken() {
		return
	}

	cq.scorer.UpdateMute(isMuted)
}

func (cq *ConnectionQuality) AddBitrateTransitionAt(bitrate int64, at time.Time) {
	if cq.done.IsBroken() {
		return
	}

	cq.scorer.AddBitrateTransitionAt(bitrate, at)
}

func (cq *ConnectionQuality) AddBitrateTransition(bitrate int64) {
	if cq.done.IsBroken() {
		return
	}

	cq.scorer.AddBitrateTransition(bitrate)
}

func (cq *ConnectionQuality) UpdateLayerMuteAt(isMuted bool, at time.Time) {
	if cq.done.IsBroken() {
		return
	}

	cq.scorer.UpdateLayerMuteAt(isMuted, at)
}

func (cq *ConnectionQuality) UpdateLayerMute(isMuted bool) {
	if cq.done.IsBroken() {
		return
	}

	cq.scorer.UpdateLayerMute(isMuted)
}

func (cq *ConnectionQuality) UpdatePauseAt(isPaused bool, at time.Time) {
	if cq.done.IsBroken() {
		return
	}

	cq.scorer.UpdatePauseAt(isPaused, at)
}

func (cq *ConnectionQuality) UpdatePause(isPaused bool) {
	if cq.done.IsBroken() {
		return
	}

	cq.scorer.UpdatePause(isPaused)
}

func (cq *ConnectionQuality) AddLayerTransitionAt(distance float64, at time.Time) {
	if cq.done.IsBroken() {
		return
	}

	cq.scorer.AddLayerTransitionAt(distance, at)
}

func (cq *ConnectionQuality) AddLayerTransition(distance float64) {
	if cq.done.IsBroken() {
		return
	}

	cq.scorer.AddLayerTransition(distance)
}

func (cq *ConnectionQuality) GetScoreAndQuality() (float32, livekit.ConnectionQuality) {
	return cq.scorer.GetMOSAndQuality()
}

func (cq *ConnectionQuality) updateScoreWithAggregate(agg *rtpstats.RTPDeltaInfo, lastRTCPAt time.Time, at time.Time) {
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
		cq.scorer.Update(&stat)
	} else {
		cq.scorer.UpdateAt(&stat, at)
	}
}

func (cq *ConnectionQuality) updateScoreFromReceiverReport(at time.Time) {
	if cq.params.SenderProvider == nil {
		return
	}

	streamingStartedAt := cq.updateStreamingStart(at)
	if streamingStartedAt.IsZero() {
		// not streaming
		return
	}

	streams := cq.params.SenderProvider.GetConnectionQualityDeltaStatsSender()
	if len(streams) == 0 {
		//  check for receiver report not received for a while
		marker := cq.params.SenderProvider.GetPrimaryStreamLastReceiverReportTime()
		if marker.IsZero() || streamingStartedAt.After(marker) {
			marker = streamingStartedAt
		}
		if time.Since(marker) > noReceiverReportTooLongThreshold {
			// have not received receiver report for a long time when streaming, run with nil stat
			cq.updateScoreWithAggregate(nil, time.Time{}, at)
			return
		}

		// wait for receiver report to process
		return
	}

	// delta stat duration could be large due to not receiving receiver report for a long time (for example, due to mute),
	// adjust to streaming start if necessary
	if streamingStartedAt.After(cq.params.SenderProvider.GetPrimaryStreamLastReceiverReportTime()) {
		// last receiver report was before streaming started, wait for next one
		return
	}

	agg := toAggregateDeltaInfo(streams)
	if streamingStartedAt.After(agg.StartTime) {
		agg.StartTime = streamingStartedAt
	}
	cq.updateScoreWithAggregate(agg, time.Time{}, at)
}

func (cq *ConnectionQuality) updateScoreAt(at time.Time) {
	if cq.params.SenderProvider != nil {
		// receiver report based quality scoring, use stats from receiver report for scoring
		cq.updateScoreFromReceiverReport(at)
		return
	}

	if cq.params.ReceiverProvider == nil {
		return
	}

	streams := cq.params.ReceiverProvider.GetConnectionQualityDeltaStats()
	if len(streams) == 0 {
		return
	}

	deltaInfoList := make([]*rtpstats.RTPDeltaInfo, 0, len(streams))
	for _, s := range streams {
		deltaInfoList = append(deltaInfoList, s.RTPStats)
	}
	agg := rtpstats.AggregateRTPDeltaInfo(deltaInfoList)
	cq.updateScoreWithAggregate(agg, cq.params.ReceiverProvider.GetLastSenderReportTime(), at)
}

func (cq *ConnectionQuality) updateStreamingStart(at time.Time) time.Time {
	cq.lock.Lock()
	defer cq.lock.Unlock()

	packetsSent := cq.params.SenderProvider.GetPrimaryStreamPacketsSent()
	if packetsSent > cq.packetsSent {
		if cq.streamingStartedAt.IsZero() {
			// the start could be anywhere after last update, but using `at` as this is not required to be accurate
			if at.IsZero() {
				cq.streamingStartedAt = time.Now()
			} else {
				cq.streamingStartedAt = at
			}
		}
	} else {
		cq.streamingStartedAt = time.Time{}
	}
	cq.packetsSent = packetsSent

	return cq.streamingStartedAt
}

func (cq *ConnectionQuality) updateScoreWorker() {
	interval := cq.params.UpdateInterval
	if interval == 0 {
		interval = updateIntervalDefault
	}

	tk := time.NewTicker(interval)
	defer tk.Stop()

	for {
		select {
		case <-cq.done.Watch():
			return

		case <-tk.C:
			if cq.done.IsBroken() {
				return
			}

			cq.updateScoreAt(time.Time{})
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
		// 5%: fall to GOOD, 15.0%: fall to POOR
		plw = 4.0
		if isFecEnabled {
			// 7.5%: fall to GOOD, 22.5%: fall to POOR
			plw /= 1.5
		}

	case strings.HasPrefix(strings.ToLower(mimeType), "video/"):
		// 2%: fall to GOOD, 6%: fall to POOR
		plw = 10.0
	}

	return plw
}

func toAggregateDeltaInfo(streams map[uint32]*buffer.StreamStatsWithLayers) *rtpstats.RTPDeltaInfo {
	deltaInfoList := make([]*rtpstats.RTPDeltaInfo, 0, len(streams))
	for _, s := range streams {
		deltaInfoList = append(deltaInfoList, s.RTPStats)
	}
	return rtpstats.AggregateRTPDeltaInfo(deltaInfoList)
}

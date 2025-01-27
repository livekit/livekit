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

package telemetry

import (
	"strings"
	"time"

	"github.com/frostbyte73/core"
	"go.uber.org/atomic"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/rtpstats"
)

const (
	updateIntervalDefault = 15 * time.Second
)

type TrackStatsProvider interface {
	GetTelemetryDeltaStats() map[uint32]*buffer.StreamStatsWithLayers
}

type TrackStatsListener interface {
	OnTrackStatsUpdate(stat *livekit.AnalyticsStat)
}

// TODO-DEPRECATE-TRACK-SCORE
type TrackScoreProvider interface {
	GetConnectionScoreAndQuality() (float32, livekit.ConnectionQuality)
}

type TrackStatsParams struct {
	UpdateInterval time.Duration
	StatsProvider  TrackStatsProvider
	StatsListener  TrackStatsListener
	ScoreProvider  TrackScoreProvider
	Logger         logger.Logger
}

type TrackStats struct {
	params TrackStatsParams

	isStarted     atomic.Bool
	codecMimeType atomic.String
	isVideo       atomic.Bool

	done core.Fuse
}

func NewTrackStats(params TrackStatsParams) *TrackStats {
	return &TrackStats{
		params: params,
	}
}

func (ts *TrackStats) Start(codecMimeType string) {
	if ts.isStarted.Swap(true) {
		return
	}

	ts.isVideo.Store(strings.HasPrefix(strings.ToLower(codecMimeType), "video/"))
	ts.codecMimeType.Store(codecMimeType)

	go ts.updateStatsWorker()
}

func (ts *TrackStats) Close() {
	ts.done.Break()
}

func (ts *TrackStats) reportStats() {
	streams := ts.params.StatsProvider.GetTelemetryDeltaStats()
	if len(streams) == 0 {
		return
	}

	analyticsStreams := make([]*livekit.AnalyticsStream, 0, len(streams))
	for ssrc, stream := range streams {
		as := toAnalyticsStream(ssrc, stream.RTPStats)

		//
		// add video layer if either
		//   1. Simulcast - even if there is only one layer per stream as it provides layer id
		//   2. A stream has multiple layers
		//
		if (len(streams) > 1 || len(stream.Layers) > 1) && ts.isVideo.Load() {
			for layer, layerStats := range stream.Layers {
				avl := toAnalyticsVideoLayer(layer, layerStats)
				if avl != nil {
					as.VideoLayers = append(as.VideoLayers, avl)
				}
			}
		}

		analyticsStreams = append(analyticsStreams, as)
	}

	score, _ := ts.params.ScoreProvider.GetConnectionScoreAndQuality()
	ts.params.StatsListener.OnTrackStatsUpdate(&livekit.AnalyticsStat{
		Score:   score,
		Streams: analyticsStreams,
		Mime:    ts.codecMimeType.Load(),
	})
}

func (ts *TrackStats) updateStatsWorker() {
	interval := ts.params.UpdateInterval
	if interval == 0 {
		interval = updateIntervalDefault
	}

	tk := time.NewTicker(interval)
	defer tk.Stop()

	for {
		select {
		case <-ts.done.Watch():
			return

		case <-tk.C:
			if ts.done.IsBroken() {
				return
			}

			ts.reportStats()
		}
	}
}

// -----------------------------------------------------------------------

func toAnalyticsStream(ssrc uint32, deltaStats *rtpstats.RTPDeltaInfo) *livekit.AnalyticsStream {
	// discount the feed side loss when reporting forwarded track stats
	packetsLost := deltaStats.PacketsLost
	if deltaStats.PacketsMissing > packetsLost {
		packetsLost = 0
	} else {
		packetsLost -= deltaStats.PacketsMissing
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
		Rtt:               deltaStats.RttMax,
		Jitter:            uint32(deltaStats.JitterMax),
		Nacks:             deltaStats.Nacks,
		Plis:              deltaStats.Plis,
		Firs:              deltaStats.Firs,
	}
}

func toAnalyticsVideoLayer(layer int32, layerStats *rtpstats.RTPDeltaInfo) *livekit.AnalyticsVideoLayer {
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

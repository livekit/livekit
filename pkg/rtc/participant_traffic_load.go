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

package rtc

import (
	"sync"

	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/telemetry"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type ParticipantTrafficLoadParams struct {
	Participant      *ParticipantImpl
	DataChannelStats *telemetry.BytesTrackStats
	Logger           logger.Logger
}

type ParticipantTrafficLoad struct {
	params ParticipantTrafficLoadParams

	lock               sync.Mutex
	tracksStatsMedia   map[livekit.TrackID]*livekit.RTPStats
	dataChannelTraffic *telemetry.TrafficTotals
}

func NewParticipantTrafficLoad(params ParticipantTrafficLoadParams) *ParticipantTrafficLoad {
	return &ParticipantTrafficLoad{
		params:           params,
		tracksStatsMedia: make(map[livekit.TrackID]*livekit.RTPStats),
	}
}

func (p *ParticipantTrafficLoad) GetTrafficLoad() *livekit.TrafficLoad {
	publishedTracks := p.params.Participant.GetPublishedTracks()
	subscribedTracks := p.params.Participant.SubscriptionManager.GetSubscribedTracks()

	availableTracks := make(map[livekit.TrackID]bool, len(publishedTracks)+len(subscribedTracks))

	upstreamAudioStats := make([]*livekit.TrafficStats, 0, len(publishedTracks))
	upstreamVideoStats := make([]*livekit.TrafficStats, 0, len(publishedTracks))

	downstreamAudioStats := make([]*livekit.TrafficStats, 0, len(subscribedTracks))
	downstreamVideoStats := make([]*livekit.TrafficStats, 0, len(subscribedTracks))

	p.lock.Lock()
	defer p.lock.Lock()
	for _, pt := range publishedTracks {
		lmt := pt.(types.LocalMediaTrack)
		trackID := lmt.ID()
		stats := lmt.GetTrackStats()
		trafficStats := utils.RTPStatsDiffToTrafficStats(p.tracksStatsMedia[trackID], stats)
		if stats != nil {
			p.tracksStatsMedia[trackID] = stats
			availableTracks[trackID] = true
		}
		if trafficStats != nil {
			switch lmt.Kind() {
			case livekit.TrackType_AUDIO:
				upstreamAudioStats = append(upstreamAudioStats, trafficStats)
			case livekit.TrackType_VIDEO:
				upstreamVideoStats = append(upstreamVideoStats, trafficStats)
			}
		}
	}

	for _, st := range subscribedTracks {
		trackID := st.ID()
		stats := st.DownTrack().GetTrackStats()
		trafficStats := utils.RTPStatsDiffToTrafficStats(p.tracksStatsMedia[trackID], stats)
		if stats != nil {
			p.tracksStatsMedia[trackID] = stats
			availableTracks[trackID] = true
		}
		if trafficStats != nil {
			switch st.MediaTrack().Kind() {
			case livekit.TrackType_AUDIO:
				downstreamAudioStats = append(downstreamAudioStats, trafficStats)
			case livekit.TrackType_VIDEO:
				downstreamVideoStats = append(downstreamVideoStats, trafficStats)
			}
		}
	}

	// remove unavailable tracks from track stats cache
	for trackID := range p.tracksStatsMedia {
		if !availableTracks[trackID] {
			delete(p.tracksStatsMedia, trackID)
		}
	}

	trafficTypeStats := make([]*livekit.TrafficTypeStats, 0, 6)
	addTypeStats := func(statsList []*livekit.TrafficStats, trackType livekit.TrackType, streamType livekit.StreamType) {
		agg := utils.AggregateTrafficStats(statsList)
		if agg != nil {
			trafficTypeStats = append(trafficTypeStats, &livekit.TrafficTypeStats{
				TrackType:    trackType,
				StreamType:   streamType,
				TrafficStats: agg,
			})
		}
	}
	addTypeStats(upstreamAudioStats, livekit.TrackType_AUDIO, livekit.StreamType_UPSTREAM)
	addTypeStats(upstreamVideoStats, livekit.TrackType_VIDEO, livekit.StreamType_UPSTREAM)
	addTypeStats(downstreamAudioStats, livekit.TrackType_VIDEO, livekit.StreamType_DOWNSTREAM)
	addTypeStats(downstreamVideoStats, livekit.TrackType_VIDEO, livekit.StreamType_DOWNSTREAM)

	if p.params.DataChannelStats != nil {
		dataChannelTraffic := p.params.DataChannelStats.GetTrafficTotals()
		if p.dataChannelTraffic != nil {
			trafficTypeStats = append(trafficTypeStats, &livekit.TrafficTypeStats{
				TrackType:  livekit.TrackType_DATA,
				StreamType: livekit.StreamType_UPSTREAM,
				TrafficStats: &livekit.TrafficStats{
					StartTime: timestamppb.New(p.dataChannelTraffic.At),
					EndTime:   timestamppb.New(dataChannelTraffic.At),
					Packets:   dataChannelTraffic.RecvMessages - p.dataChannelTraffic.RecvMessages,
					Bytes:     dataChannelTraffic.RecvBytes - p.dataChannelTraffic.RecvBytes,
				},
			})

			trafficTypeStats = append(trafficTypeStats, &livekit.TrafficTypeStats{
				TrackType:  livekit.TrackType_DATA,
				StreamType: livekit.StreamType_DOWNSTREAM,
				TrafficStats: &livekit.TrafficStats{
					StartTime: timestamppb.New(p.dataChannelTraffic.At),
					EndTime:   timestamppb.New(dataChannelTraffic.At),
					Packets:   dataChannelTraffic.SendMessages - p.dataChannelTraffic.SendMessages,
					Bytes:     dataChannelTraffic.SendBytes - p.dataChannelTraffic.SendBytes,
				},
			})
		}
		p.dataChannelTraffic = dataChannelTraffic
	}

	return &livekit.TrafficLoad{
		TrafficTypeStats: trafficTypeStats,
	}
}

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
	"fmt"
	"time"

	"go.uber.org/atomic"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/utils"
)

type BytesTrackType string

const (
	BytesTrackTypeData   BytesTrackType = "DT"
	BytesTrackTypeSignal BytesTrackType = "SG"
)

// stats for signal and data channel
type BytesTrackStats struct {
	trackID    livekit.TrackID
	pID        livekit.ParticipantID
	send, recv atomic.Uint64
	telemetry  TelemetryService
	isStopped  atomic.Bool
}

func NewBytesTrackStats(trackID livekit.TrackID, pID livekit.ParticipantID, telemetry TelemetryService) *BytesTrackStats {
	s := &BytesTrackStats{
		trackID:   trackID,
		pID:       pID,
		telemetry: telemetry,
	}
	go s.reporter()
	return s
}

func (s *BytesTrackStats) AddBytes(bytes uint64, isSend bool) {
	if isSend {
		s.send.Add(bytes)
	} else {
		s.recv.Add(bytes)
	}
}

func (s *BytesTrackStats) Stop() {
	s.isStopped.Store(true)
}

func (s *BytesTrackStats) report() {
	if recv := s.recv.Swap(0); recv > 0 {
		s.telemetry.TrackStats(StatsKeyForData(livekit.StreamType_UPSTREAM, s.pID, s.trackID), &livekit.AnalyticsStat{
			Streams: []*livekit.AnalyticsStream{
				{PrimaryBytes: recv},
			},
		})
	}

	if send := s.send.Swap(0); send > 0 {
		s.telemetry.TrackStats(StatsKeyForData(livekit.StreamType_DOWNSTREAM, s.pID, s.trackID), &livekit.AnalyticsStat{
			Streams: []*livekit.AnalyticsStream{
				{PrimaryBytes: send},
			},
		})
	}
}

func (s *BytesTrackStats) reporter() {
	ticker := time.NewTicker(config.TelemetryStatsUpdateInterval)
	defer ticker.Stop()

	for !s.isStopped.Load() {
		<-ticker.C
		s.report()
	}

	s.report()
}

// -----------------------------------------------------------------------

func BytesTrackIDForParticipantID(typ BytesTrackType, participantID livekit.ParticipantID) livekit.TrackID {
	return livekit.TrackID(fmt.Sprintf("%s_%s%s", utils.TrackPrefix, string(typ), participantID))
}

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
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/frostbyte73/core"
	"go.uber.org/atomic"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/utils"
)

type BytesTrackType string

const (
	BytesTrackTypeData   BytesTrackType = "DT"
	BytesTrackTypeSignal BytesTrackType = "SG"
)

// -------------------------------

type TrafficTotals struct {
	At           time.Time
	SendBytes    uint64
	SendMessages uint32
	RecvBytes    uint64
	RecvMessages uint32
}

// --------------------------------

// stats for signal and data channel
type BytesTrackStats struct {
	trackID                              livekit.TrackID
	pID                                  livekit.ParticipantID
	send, recv                           atomic.Uint64
	sendMessages, recvMessages           atomic.Uint32
	totalSendBytes, totalRecvBytes       atomic.Uint64
	totalSendMessages, totalRecvMessages atomic.Uint32
	telemetry                            TelemetryService
	done                                 core.Fuse
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
		s.sendMessages.Inc()
		s.totalSendBytes.Add(bytes)
		s.totalSendMessages.Inc()
	} else {
		s.recv.Add(bytes)
		s.recvMessages.Inc()
		s.totalRecvBytes.Add(bytes)
		s.totalRecvMessages.Inc()
	}
}

func (s *BytesTrackStats) GetTrafficTotals() *TrafficTotals {
	return &TrafficTotals{
		At:           time.Now(),
		SendBytes:    s.totalSendBytes.Load(),
		SendMessages: s.totalSendMessages.Load(),
		RecvBytes:    s.totalRecvBytes.Load(),
		RecvMessages: s.totalRecvMessages.Load(),
	}
}

func (s *BytesTrackStats) Stop() {
	s.done.Break()
}

func (s *BytesTrackStats) report() {
	if recv := s.recv.Swap(0); recv > 0 {
		s.telemetry.TrackStats(StatsKeyForData(livekit.StreamType_UPSTREAM, s.pID, s.trackID), &livekit.AnalyticsStat{
			Streams: []*livekit.AnalyticsStream{
				{
					PrimaryBytes:   recv,
					PrimaryPackets: s.recvMessages.Swap(0),
				},
			},
		})
	}

	if send := s.send.Swap(0); send > 0 {
		s.telemetry.TrackStats(StatsKeyForData(livekit.StreamType_DOWNSTREAM, s.pID, s.trackID), &livekit.AnalyticsStat{
			Streams: []*livekit.AnalyticsStream{
				{
					PrimaryBytes:   send,
					PrimaryPackets: s.sendMessages.Swap(0),
				},
			},
		})
	}
}

func (s *BytesTrackStats) reporter() {
	ticker := time.NewTicker(5 * time.Second)
	defer func() {
		ticker.Stop()
		s.report()
	}()

	for {
		select {
		case <-s.done.Watch():
			return
		case <-ticker.C:
			s.report()
		}
	}
}

// -----------------------------------------------------------------------

type BytesSignalStats struct {
	BytesTrackStats
	ctx context.Context

	mu sync.Mutex
	ri *livekit.Room
	pi *livekit.ParticipantInfo
}

func NewBytesSignalStats(ctx context.Context, telemetry TelemetryService) *BytesSignalStats {
	return &BytesSignalStats{
		BytesTrackStats: BytesTrackStats{
			telemetry: telemetry,
		},
		ctx: ctx,
	}
}

func (s *BytesSignalStats) ResolveRoom(ri *livekit.Room) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ri == nil && ri.GetSid() != "" {
		s.ri = &livekit.Room{
			Sid:  ri.Sid,
			Name: ri.Name,
		}
		s.maybeStart()
	}
}

func (s *BytesSignalStats) ResolveParticipant(pi *livekit.ParticipantInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pi == nil && pi != nil {
		s.pi = &livekit.ParticipantInfo{
			Sid:      pi.Sid,
			Identity: pi.Identity,
		}
		s.maybeStart()
	}
}

func (s *BytesSignalStats) maybeStart() {
	if s.ri == nil || s.pi == nil {
		return
	}

	s.pID = livekit.ParticipantID(s.pi.Sid)
	s.trackID = BytesTrackIDForParticipantID(BytesTrackTypeSignal, s.pID)

	s.telemetry.ParticipantJoined(s.ctx, s.ri, s.pi, nil, nil, false)
	go s.reporter()
}

func (s *BytesSignalStats) reporter() {
	s.BytesTrackStats.reporter()
	s.telemetry.ParticipantLeft(s.ctx, s.ri, s.pi, false)
}

// -----------------------------------------------------------------------

func BytesTrackIDForParticipantID(typ BytesTrackType, participantID livekit.ParticipantID) livekit.TrackID {
	return livekit.TrackID(fmt.Sprintf("%s_%s%s", utils.TrackPrefix, string(typ), participantID))
}

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
	"github.com/livekit/protocol/observability/roomobs"
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
	country                              string
	trackID                              livekit.TrackID
	pID                                  livekit.ParticipantID
	send, recv                           atomic.Uint64
	sendMessages, recvMessages           atomic.Uint32
	totalSendBytes, totalRecvBytes       atomic.Uint64
	totalSendMessages, totalRecvMessages atomic.Uint32
	telemetry                            TelemetryService
	reporter                             roomobs.TrackReporter
	done                                 core.Fuse
}

func NewBytesTrackStats(
	country string,
	trackID livekit.TrackID,
	pID livekit.ParticipantID,
	telemetry TelemetryService,
	participantReporter roomobs.ParticipantSessionReporter,
) *BytesTrackStats {
	s := &BytesTrackStats{
		country:   country,
		trackID:   trackID,
		pID:       pID,
		telemetry: telemetry,
		reporter:  participantReporter.WithTrack(trackID.String()),
	}
	go s.worker()
	return s
}

func (s *BytesTrackStats) AddBytes(bytes uint64, isSend bool) {
	if isSend {
		s.send.Add(bytes)
		s.sendMessages.Inc()
		s.totalSendBytes.Add(bytes)
		s.totalSendMessages.Inc()

		s.reporter.Tx(func(tx roomobs.TrackTx) {
			tx.ReportType(roomobs.TrackTypeData)
			tx.ReportSendBytes(uint32(bytes))
			tx.ReportSendPackets(1)
		})
	} else {
		s.recv.Add(bytes)
		s.recvMessages.Inc()
		s.totalRecvBytes.Add(bytes)
		s.totalRecvMessages.Inc()

		s.reporter.Tx(func(tx roomobs.TrackTx) {
			tx.ReportType(roomobs.TrackTypeData)
			tx.ReportRecvBytes(uint32(bytes))
			tx.ReportRecvPackets(1)
		})
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
		packets := s.recvMessages.Swap(0)
		s.telemetry.TrackStats(
			StatsKeyForData(s.country, livekit.StreamType_UPSTREAM, s.pID, s.trackID),
			&livekit.AnalyticsStat{
				Streams: []*livekit.AnalyticsStream{
					{
						PrimaryBytes:   recv,
						PrimaryPackets: packets,
					},
				},
			},
		)
	}

	if send := s.send.Swap(0); send > 0 {
		packets := s.sendMessages.Swap(0)
		s.telemetry.TrackStats(
			StatsKeyForData(s.country, livekit.StreamType_DOWNSTREAM, s.pID, s.trackID),
			&livekit.AnalyticsStat{
				Streams: []*livekit.AnalyticsStream{
					{
						PrimaryBytes:   send,
						PrimaryPackets: packets,
					},
				},
			},
		)
	}
}

func (s *BytesTrackStats) worker() {
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

	guard ReferenceGuard

	participantResolver roomobs.ParticipantReporterResolver
	trackResolver       roomobs.KeyResolver

	mu      sync.Mutex
	ri      *livekit.Room
	pi      *livekit.ParticipantInfo
	stopped chan struct{}
}

func NewBytesSignalStats(
	ctx context.Context,
	telemetry TelemetryService,
) *BytesSignalStats {
	projectReporter := telemetry.RoomProjectReporter(ctx)
	participantReporter, participantReporterResolver := roomobs.DeferredParticipantReporter(projectReporter)
	trackReporter, trackReporterResolver := participantReporter.WithDeferredTrack()
	return &BytesSignalStats{
		BytesTrackStats: BytesTrackStats{
			telemetry: telemetry,
			reporter:  trackReporter,
		},
		ctx:                 ctx,
		participantResolver: participantReporterResolver,
		trackResolver:       trackReporterResolver,
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

func (s *BytesSignalStats) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped != nil {
		s.done.Break()
		<-s.stopped
		s.stopped = nil
		s.done = core.Fuse{}
	}
	s.ri = nil
	s.pi = nil

	s.participantResolver.Reset()
	s.trackResolver.Reset()
}

func (s *BytesSignalStats) maybeStart() {
	if s.ri == nil || s.pi == nil {
		return
	}

	s.pID = livekit.ParticipantID(s.pi.Sid)
	s.trackID = BytesTrackIDForParticipantID(BytesTrackTypeSignal, s.pID)

	s.participantResolver.Resolve(
		livekit.RoomName(s.ri.Name),
		livekit.RoomID(s.ri.Sid),
		livekit.ParticipantIdentity(s.pi.Identity),
		livekit.ParticipantID(s.pi.Sid),
	)
	s.trackResolver.Resolve(string(s.trackID))

	s.telemetry.ParticipantJoined(s.ctx, s.ri, s.pi, nil, nil, false, &s.guard)
	s.stopped = make(chan struct{})
	go s.worker()
}

func (s *BytesSignalStats) worker() {
	s.BytesTrackStats.worker()
	s.telemetry.ParticipantLeft(s.ctx, s.ri, s.pi, false, &s.guard)
	close(s.stopped)
}

// -----------------------------------------------------------------------

func BytesTrackIDForParticipantID(typ BytesTrackType, participantID livekit.ParticipantID) livekit.TrackID {
	return livekit.TrackID(fmt.Sprintf("%s%s%s", utils.TrackPrefix, typ, participantID))
}

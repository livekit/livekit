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
	"sync"
	"time"
	"unsafe"

	"go.uber.org/zap/zapcore"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	protoutils "github.com/livekit/protocol/utils"
)

type ReferenceGuard struct {
	activated, released bool
}

func (r *ReferenceGuard) MarshalLogObject(e zapcore.ObjectEncoder) error {
	e.AddUintptr("self", uintptr(unsafe.Pointer(r)))
	e.AddBool("activated", r.activated)
	e.AddBool("released", r.released)
	return nil
}

// ----------------------------------------

type ReferenceCount struct {
	count int
}

func (s *ReferenceCount) Activate(guard *ReferenceGuard) {
	if guard != nil && !guard.activated {
		guard.activated = true
		s.count++
	}
}

func (s *ReferenceCount) Release(guard *ReferenceGuard) bool {
	if guard == nil || !guard.activated || guard.released {
		return false
	}
	guard.released = true
	s.count--
	return s.count == 0
}

// Take hands over every reference held, leaving none behind.
func (s *ReferenceCount) Take() int {
	count := s.count
	s.count = 0
	return count
}

// Absorb takes on references handed over from elsewhere.
func (s *ReferenceCount) Absorb(count int) {
	s.count += count
}

func (s ReferenceCount) MarshalLogObject(e zapcore.ObjectEncoder) error {
	e.AddInt("count", s.count)
	return nil
}

// ----------------------------------------

// statsBatch is stats collected while the worker was in one room
type statsBatch struct {
	roomID           livekit.RoomID
	roomName         livekit.RoomName
	incomingPerTrack map[livekit.TrackID][]*livekit.AnalyticsStat
	outgoingPerTrack map[livekit.TrackID][]*livekit.AnalyticsStat
}

func (b statsBatch) isEmpty() bool {
	return len(b.incomingPerTrack) == 0 && len(b.outgoingPerTrack) == 0
}

// ----------------------------------------

// StatsWorker handles participant stats
type StatsWorker struct {
	next *StatsWorker

	ctx                 context.Context
	t                   TelemetryService
	participantID       livekit.ParticipantID
	participantIdentity livekit.ParticipantIdentity

	lock sync.RWMutex
	// the room a worker belongs to can change mid-session, so it is mutable state
	// guarded by `lock`. it is kept in sync with the key the worker is filed under in
	// telemetryService.workers, see telemetryService.reKeyRoom.
	roomID   livekit.RoomID
	roomName livekit.RoomName
	// batches sealed off by a room change, they carry the room they were collected
	// in and go out on the next flush
	sealed           []statsBatch
	isConnected      bool
	outgoingPerTrack map[livekit.TrackID][]*livekit.AnalyticsStat
	incomingPerTrack map[livekit.TrackID][]*livekit.AnalyticsStat
	refCount         ReferenceCount
	closedAt         time.Time
}

func newStatsWorker(
	ctx context.Context,
	t TelemetryService,
	roomID livekit.RoomID,
	roomName livekit.RoomName,
	participantID livekit.ParticipantID,
	identity livekit.ParticipantIdentity,
	guard *ReferenceGuard,
) *StatsWorker {
	s := &StatsWorker{
		ctx:                 ctx,
		t:                   t,
		roomID:              roomID,
		roomName:            roomName,
		participantID:       participantID,
		participantIdentity: identity,
		outgoingPerTrack:    make(map[livekit.TrackID][]*livekit.AnalyticsStat),
		incomingPerTrack:    make(map[livekit.TrackID][]*livekit.AnalyticsStat),
	}
	s.refCount.Activate(guard)
	return s
}

func (s *StatsWorker) OnTrackStat(trackID livekit.TrackID, direction livekit.StreamType, stat *livekit.AnalyticsStat) {
	s.lock.Lock()
	if direction == livekit.StreamType_DOWNSTREAM {
		s.outgoingPerTrack[trackID] = append(s.outgoingPerTrack[trackID], stat)
	} else {
		s.incomingPerTrack[trackID] = append(s.incomingPerTrack[trackID], stat)
	}
	s.lock.Unlock()
}

func (s *StatsWorker) ParticipantID() livekit.ParticipantID {
	return s.participantID
}

func (s *StatsWorker) RoomID() livekit.RoomID {
	s.lock.RLock()
	defer s.lock.RUnlock()

	return s.roomID
}

// SetRoom re-points the worker at a room.
//
// Stats collected so far are sealed off rather than re-stamped - a room id changes
// because the previous session ended, so what was collected under it belongs to it.
// Sealing keeps the re-key free of any sending, the sealed stats go out on the next
// flush like every other stat.
func (s *StatsWorker) SetRoom(roomID livekit.RoomID, roomName livekit.RoomName) {
	s.lock.Lock()
	defer s.lock.Unlock()

	if s.roomID == roomID && s.roomName == roomName {
		return
	}

	if batch := s.sealStatsLocked(); !batch.isEmpty() {
		s.sealed = append(s.sealed, batch)
	}

	s.roomID = roomID
	s.roomName = roomName
}

// sealStatsLocked hands over everything collected since the last seal, stamped
// with the room it was collected in
func (s *StatsWorker) sealStatsLocked() statsBatch {
	batch := statsBatch{
		roomID:           s.roomID,
		roomName:         s.roomName,
		incomingPerTrack: s.incomingPerTrack,
		outgoingPerTrack: s.outgoingPerTrack,
	}

	s.incomingPerTrack = make(map[livekit.TrackID][]*livekit.AnalyticsStat)
	s.outgoingPerTrack = make(map[livekit.TrackID][]*livekit.AnalyticsStat)

	return batch
}

func (s *StatsWorker) SetConnected() {
	s.lock.Lock()
	s.isConnected = true
	s.lock.Unlock()
}

func (s *StatsWorker) IsConnected() bool {
	s.lock.RLock()
	defer s.lock.RUnlock()

	return s.isConnected
}

func (s *StatsWorker) Flush(now time.Time, closeWait time.Duration) bool {
	ts := timestamppb.New(now)

	s.lock.Lock()
	// anything sealed off by a room change goes out along with the current batch,
	// each stamped with the room it was collected in
	batches := append(s.sealed, s.sealStatsLocked())
	s.sealed = nil

	closed := !s.closedAt.IsZero() && now.Sub(s.closedAt) > closeWait
	s.lock.Unlock()

	numTracks := 0
	for _, batch := range batches {
		numTracks += len(batch.incomingPerTrack) + len(batch.outgoingPerTrack)
	}

	stats := make([]*livekit.AnalyticsStat, 0, numTracks)
	for _, batch := range batches {
		stats = s.collectStats(ts, batch, livekit.StreamType_UPSTREAM, stats)
		stats = s.collectStats(ts, batch, livekit.StreamType_DOWNSTREAM, stats)
	}
	if len(stats) > 0 {
		s.t.SendStats(s.ctx, stats)
	}

	return closed
}

func (s *StatsWorker) Close(guard *ReferenceGuard) bool {
	s.lock.Lock()
	defer s.lock.Unlock()

	if !s.refCount.Release(guard) {
		return false
	}

	ok := s.closedAt.IsZero()
	if ok {
		s.closedAt = time.Now()
	}
	return ok
}

// ForceClose closes the worker irrespective of outstanding references. Used when a
// worker can no longer be reached through the worker map, so that it drains and is
// reaped instead of lingering in the flush list forever.
//
// Its references are handed over to `successor`, the worker that can be reached in its
// place, so that whoever holds one still has a live worker to close. A ReferenceGuard
// records that it activated some worker, not which one, so leaving them behind would
// strand the successor with references it can never see released.
func (s *StatsWorker) ForceClose(successor *StatsWorker) bool {
	s.lock.Lock()
	if !s.closedAt.IsZero() {
		s.lock.Unlock()
		return false
	}

	s.closedAt = time.Now()
	count := s.refCount.Take()
	s.lock.Unlock()

	if successor != nil && count != 0 {
		successor.lock.Lock()
		successor.refCount.Absorb(count)
		successor.lock.Unlock()
	}

	return true
}

func (s *StatsWorker) Closed(guard *ReferenceGuard) bool {
	s.lock.Lock()
	defer s.lock.Unlock()
	if s.closedAt.IsZero() {
		s.refCount.Activate(guard)
		return false
	}
	return true
}

func (s *StatsWorker) collectStats(
	ts *timestamppb.Timestamp,
	batch statsBatch,
	streamType livekit.StreamType,
	stats []*livekit.AnalyticsStat,
) []*livekit.AnalyticsStat {
	perTrack := batch.incomingPerTrack
	if streamType == livekit.StreamType_DOWNSTREAM {
		perTrack = batch.outgoingPerTrack
	}

	for trackID, analyticsStats := range perTrack {
		coalesced := coalesce(analyticsStats)
		if coalesced == nil {
			continue
		}

		coalesced.TimeStamp = ts
		coalesced.TrackId = string(trackID)
		coalesced.Kind = streamType
		coalesced.RoomId = string(batch.roomID)
		coalesced.ParticipantId = string(s.participantID)
		coalesced.RoomName = string(batch.roomName)
		stats = append(stats, coalesced)
	}
	return stats
}

func (s *StatsWorker) MarshalLogObject(e zapcore.ObjectEncoder) error {
	s.lock.RLock()
	defer s.lock.RUnlock()

	e.AddString("room", string(s.roomName))
	e.AddString("roomID", string(s.roomID))
	e.AddString("participant", string(s.participantIdentity))
	e.AddString("participantID", string(s.participantID))
	e.AddBool("isConnected", s.isConnected)
	e.AddTime("closedAt", s.closedAt)
	e.AddObject("refCount", s.refCount)
	return nil
}

// -------------------------------------------------------------------------

// create a single stream and single video layer post aggregation
func coalesce(stats []*livekit.AnalyticsStat) *livekit.AnalyticsStat {
	if len(stats) == 0 {
		return nil
	}

	// find aggregates across streams
	startTime := time.Time{}
	endTime := time.Time{}
	scoreSum := float32(0.0) // used for average
	minScore := float32(0.0) // min score in batched stats
	var scores []float32     // used for median
	maxRtt := uint32(0)
	maxJitter := uint32(0)
	coalescedVideoLayers := make(map[int32]*livekit.AnalyticsVideoLayer)
	coalescedStream := &livekit.AnalyticsStream{}
	for _, stat := range stats {
		if !isValid(stat) {
			logger.Warnw("telemetry skipping invalid stat", nil, "stat", stat)
			continue
		}

		// only consider non-zero scores
		if stat.Score > 0 {
			if minScore == 0 {
				minScore = stat.Score
			} else if stat.Score < minScore {
				minScore = stat.Score
			}
			scoreSum += stat.Score
			scores = append(scores, stat.Score)
		}

		for _, analyticsStream := range stat.Streams {
			start := analyticsStream.StartTime.AsTime()
			if startTime.IsZero() || startTime.After(start) {
				startTime = start
			}

			end := analyticsStream.EndTime.AsTime()
			if endTime.IsZero() || endTime.Before(end) {
				endTime = end
			}

			if analyticsStream.Rtt > maxRtt {
				maxRtt = analyticsStream.Rtt
			}

			if analyticsStream.Jitter > maxJitter {
				maxJitter = analyticsStream.Jitter
			}

			coalescedStream.PrimaryPackets += analyticsStream.PrimaryPackets
			coalescedStream.PrimaryBytes += analyticsStream.PrimaryBytes
			coalescedStream.RetransmitPackets += analyticsStream.RetransmitPackets
			coalescedStream.RetransmitBytes += analyticsStream.RetransmitBytes
			coalescedStream.PaddingPackets += analyticsStream.PaddingPackets
			coalescedStream.PaddingBytes += analyticsStream.PaddingBytes
			coalescedStream.PacketsLost += analyticsStream.PacketsLost
			coalescedStream.PacketsOutOfOrder += analyticsStream.PacketsOutOfOrder
			coalescedStream.Frames += analyticsStream.Frames
			coalescedStream.Nacks += analyticsStream.Nacks
			coalescedStream.Plis += analyticsStream.Plis
			coalescedStream.Firs += analyticsStream.Firs

			for _, videoLayer := range analyticsStream.VideoLayers {
				coalescedVideoLayer := coalescedVideoLayers[videoLayer.Layer]
				if coalescedVideoLayer == nil {
					coalescedVideoLayer = protoutils.CloneProto(videoLayer)
					coalescedVideoLayers[videoLayer.Layer] = coalescedVideoLayer
				} else {
					coalescedVideoLayer.Packets += videoLayer.Packets
					coalescedVideoLayer.Bytes += videoLayer.Bytes
					coalescedVideoLayer.Frames += videoLayer.Frames
				}
			}
		}
	}
	coalescedStream.StartTime = timestamppb.New(startTime)
	coalescedStream.EndTime = timestamppb.New(endTime)
	coalescedStream.Rtt = maxRtt
	coalescedStream.Jitter = maxJitter

	// whittle it down to one video layer, just the max available layer
	maxVideoLayer := int32(-1)
	for _, coalescedVideoLayer := range coalescedVideoLayers {
		if maxVideoLayer == -1 || maxVideoLayer < coalescedVideoLayer.Layer {
			maxVideoLayer = coalescedVideoLayer.Layer
			coalescedStream.VideoLayers = []*livekit.AnalyticsVideoLayer{coalescedVideoLayer}
		}
	}

	stat := &livekit.AnalyticsStat{
		MinScore:    minScore,
		MedianScore: utils.Median(scores),
		Streams:     []*livekit.AnalyticsStream{coalescedStream},
		Mime:        stats[len(stats)-1].Mime, // use the latest Mime
	}
	numScores := len(scores)
	if numScores > 0 {
		stat.Score = scoreSum / float32(numScores)
	}
	return stat
}

type CondensedStat struct {
	StartTime   time.Time
	EndTime     time.Time
	Bytes       uint64
	Packets     uint32
	PacketsLost uint32
	Frames      uint32
}

func CondenseStat(stat *livekit.AnalyticsStat) (ps CondensedStat, ok bool) {
	if ok = isValid(stat); !ok {
		return
	}

	for _, stream := range stat.Streams {
		startTime := stream.StartTime.AsTime()
		endTime := stream.EndTime.AsTime()
		if ps.StartTime.IsZero() || startTime.Before(ps.StartTime) {
			ps.StartTime = startTime
		}
		if endTime.After(ps.EndTime) {
			ps.EndTime = endTime
		}

		ps.Bytes += stream.PrimaryBytes
		ps.Packets += stream.PrimaryPackets
		ps.PacketsLost += stream.PacketsLost
		if stream.Frames > ps.Frames {
			ps.Frames = stream.Frames
		}
	}

	return
}

func isValid(stat *livekit.AnalyticsStat) bool {
	for _, analyticsStream := range stat.Streams {
		if int32(analyticsStream.PrimaryPackets) < 0 ||
			int64(analyticsStream.PrimaryBytes) < 0 ||
			int32(analyticsStream.RetransmitPackets) < 0 ||
			int64(analyticsStream.RetransmitBytes) < 0 ||
			int32(analyticsStream.PaddingPackets) < 0 ||
			int64(analyticsStream.PaddingBytes) < 0 ||
			int32(analyticsStream.PacketsLost) < 0 ||
			int32(analyticsStream.PacketsOutOfOrder) < 0 ||
			int32(analyticsStream.Frames) < 0 ||
			int32(analyticsStream.Nacks) < 0 ||
			int32(analyticsStream.Plis) < 0 ||
			int32(analyticsStream.Firs) < 0 {
			return false
		}

		for _, videoLayer := range analyticsStream.VideoLayers {
			if int32(videoLayer.Packets) < 0 ||
				int64(videoLayer.Bytes) < 0 ||
				int32(videoLayer.Frames) < 0 {
				return false
			}
		}
	}

	return true
}

// Copyright 2026 LiveKit, Inc.
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

package service

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/rtc/types/typesfakes"
)

type memoryParticipantStore struct {
	lock         sync.Mutex
	participants map[livekit.ParticipantIdentity]*livekit.ParticipantInfo
}

func (s *memoryParticipantStore) StoreParticipant(_ context.Context, _ livekit.RoomName, participant *livekit.ParticipantInfo) error {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.participants[livekit.ParticipantIdentity(participant.Identity)] = participant
	return nil
}

func (s *memoryParticipantStore) DeleteParticipant(_ context.Context, _ livekit.RoomName, identity livekit.ParticipantIdentity) error {
	s.lock.Lock()
	defer s.lock.Unlock()
	delete(s.participants, identity)
	return nil
}

func TestParticipantRecords(t *testing.T) {
	ctx := context.Background()

	t.Run("does not write back a participant that left", func(t *testing.T) {
		store := &memoryParticipantStore{participants: make(map[livekit.ParticipantIdentity]*livekit.ParticipantInfo)}
		records := &participantRecords{roomName: "room", roomStore: store}
		p := &typesfakes.FakeParticipant{}
		p.ToProtoReturns(&livekit.ParticipantInfo{Sid: "PA_alice", Identity: "alice", State: livekit.ParticipantInfo_ACTIVE})

		require.NoError(t, records.store(ctx, p))
		require.Contains(t, store.participants, livekit.ParticipantIdentity("alice"))

		// the participant leaves while a restore that listed it is in flight
		p.IsDisconnectedReturns(true)
		require.NoError(t, records.delete(ctx, "alice"))
		require.NoError(t, records.store(ctx, p))

		require.NotContains(t, store.participants, livekit.ParticipantIdentity("alice"))
	})

	t.Run("does not write back a participant that closed before disconnecting", func(t *testing.T) {
		store := &memoryParticipantStore{participants: make(map[livekit.ParticipantIdentity]*livekit.ParticipantInfo)}
		records := &participantRecords{roomName: "room", roomStore: store}
		p := &typesfakes.FakeParticipant{}
		p.ToProtoReturns(&livekit.ParticipantInfo{Sid: "PA_alice", Identity: "alice", State: livekit.ParticipantInfo_ACTIVE})
		require.NoError(t, records.store(ctx, p))

		// a close callback registered while closing runs before the participant is disconnected
		p.IsClosedReturns(true)
		require.NoError(t, records.delete(ctx, "alice"))
		require.NoError(t, records.store(ctx, p))

		require.NotContains(t, store.participants, livekit.ParticipantIdentity("alice"))
	})

	t.Run("stores the participant's current state", func(t *testing.T) {
		store := &memoryParticipantStore{participants: make(map[livekit.ParticipantIdentity]*livekit.ParticipantInfo)}
		records := &participantRecords{roomName: "room", roomStore: store}
		p := &typesfakes.FakeParticipant{}
		p.ToProtoReturns(&livekit.ParticipantInfo{Identity: "alice", Metadata: "before"})
		require.NoError(t, records.store(ctx, p))

		p.ToProtoReturns(&livekit.ParticipantInfo{Identity: "alice", Metadata: "after"})
		require.NoError(t, records.store(ctx, p))

		require.Equal(t, "after", store.participants["alice"].Metadata)
	})
}

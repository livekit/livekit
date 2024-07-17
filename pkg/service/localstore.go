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

package service

import (
	"context"
	"sync"
	"time"

	"github.com/thoas/go-funk"
	"golang.org/x/exp/maps"

	"github.com/livekit/protocol/livekit"
)

// encapsulates CRUD operations for room settings
type LocalStore struct {
	// map of roomName => room
	rooms        map[livekit.RoomName]*livekit.Room
	roomInternal map[livekit.RoomName]*livekit.RoomInternal
	// map of roomName => { identity: participant }
	participants map[livekit.RoomName]map[livekit.ParticipantIdentity]*livekit.ParticipantInfo

	agentDispatches map[livekit.RoomName]*livekit.AgentDispatch
	agentJobs       map[livekit.RoomName]*livekit.Job

	lock       sync.RWMutex
	globalLock sync.Mutex
}

func NewLocalStore() *LocalStore {
	return &LocalStore{
		rooms:           make(map[livekit.RoomName]*livekit.Room),
		roomInternal:    make(map[livekit.RoomName]*livekit.RoomInternal),
		participants:    make(map[livekit.RoomName]map[livekit.ParticipantIdentity]*livekit.ParticipantInfo),
		agentDispatches: make(map[livekit.RoomName]*livekit.AgentDispatch),
		agentJobs:       make(map[livekit.RoomName]*livekit.Job),
		lock:            sync.RWMutex{},
	}
}

func (s *LocalStore) StoreRoom(_ context.Context, room *livekit.Room, internal *livekit.RoomInternal) error {
	if room.CreationTime == 0 {
		room.CreationTime = time.Now().Unix()
	}
	roomName := livekit.RoomName(room.Name)

	s.lock.Lock()
	s.rooms[roomName] = room
	s.roomInternal[roomName] = internal
	s.lock.Unlock()

	return nil
}

func (s *LocalStore) LoadRoom(_ context.Context, roomName livekit.RoomName, includeInternal bool) (*livekit.Room, *livekit.RoomInternal, error) {
	s.lock.RLock()
	defer s.lock.RUnlock()

	room := s.rooms[roomName]
	if room == nil {
		return nil, nil, ErrRoomNotFound
	}

	var internal *livekit.RoomInternal
	if includeInternal {
		internal = s.roomInternal[roomName]
	}

	return room, internal, nil
}

func (s *LocalStore) ListRooms(_ context.Context, roomNames []livekit.RoomName) ([]*livekit.Room, error) {
	s.lock.RLock()
	defer s.lock.RUnlock()
	rooms := make([]*livekit.Room, 0, len(s.rooms))
	for _, r := range s.rooms {
		if roomNames == nil || funk.Contains(roomNames, livekit.RoomName(r.Name)) {
			rooms = append(rooms, r)
		}
	}
	return rooms, nil
}

func (s *LocalStore) DeleteRoom(ctx context.Context, roomName livekit.RoomName) error {
	room, _, err := s.LoadRoom(ctx, roomName, false)
	if err == ErrRoomNotFound {
		return nil
	} else if err != nil {
		return err
	}

	s.lock.Lock()
	defer s.lock.Unlock()

	delete(s.participants, livekit.RoomName(room.Name))
	delete(s.rooms, livekit.RoomName(room.Name))
	delete(s.roomInternal, livekit.RoomName(room.Name))
	delete(s.agentDispatches, livekit.RoomName(room.Name))
	delete(s.agentJobs, livekit.RoomName(room.Name))
	return nil
}

func (s *LocalStore) LockRoom(_ context.Context, _ livekit.RoomName, _ time.Duration) (string, error) {
	// local rooms lock & unlock globally
	s.globalLock.Lock()
	return "", nil
}

func (s *LocalStore) UnlockRoom(_ context.Context, _ livekit.RoomName, _ string) error {
	s.globalLock.Unlock()
	return nil
}

func (s *LocalStore) StoreParticipant(_ context.Context, roomName livekit.RoomName, participant *livekit.ParticipantInfo) error {
	s.lock.Lock()
	defer s.lock.Unlock()
	roomParticipants := s.participants[roomName]
	if roomParticipants == nil {
		roomParticipants = make(map[livekit.ParticipantIdentity]*livekit.ParticipantInfo)
		s.participants[roomName] = roomParticipants
	}
	roomParticipants[livekit.ParticipantIdentity(participant.Identity)] = participant
	return nil
}

func (s *LocalStore) LoadParticipant(_ context.Context, roomName livekit.RoomName, identity livekit.ParticipantIdentity) (*livekit.ParticipantInfo, error) {
	s.lock.RLock()
	defer s.lock.RUnlock()

	roomParticipants := s.participants[roomName]
	if roomParticipants == nil {
		return nil, ErrParticipantNotFound
	}
	participant := roomParticipants[identity]
	if participant == nil {
		return nil, ErrParticipantNotFound
	}
	return participant, nil
}

func (s *LocalStore) ListParticipants(_ context.Context, roomName livekit.RoomName) ([]*livekit.ParticipantInfo, error) {
	s.lock.RLock()
	defer s.lock.RUnlock()

	roomParticipants := s.participants[roomName]
	if roomParticipants == nil {
		// empty array
		return nil, nil
	}

	items := make([]*livekit.ParticipantInfo, 0, len(roomParticipants))
	for _, p := range roomParticipants {
		items = append(items, p)
	}

	return items, nil
}

func (s *LocalStore) DeleteParticipant(_ context.Context, roomName livekit.RoomName, identity livekit.ParticipantIdentity) error {
	s.lock.Lock()
	defer s.lock.Unlock()

	roomParticipants := s.participants[roomName]
	if roomParticipants != nil {
		delete(roomParticipants, identity)
	}
	return nil
}

func (s *LocalStore) StoreAgentDispatch(ctx context.Context, dispatch *livekit.AgentDispatch) error {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.agentDispatches[livekit.RoomName(dispatch.Room)] = dispatch
	return nil
}

func (s *LocalStore) DeleteAgentDispatch(ctx context.Context, dispatch *livekit.AgentDispatch) error {
	s.lock.Lock()
	defer s.lock.Unlock()

	delete(s.agentDispatches, livekit.RoomName(dispatch.Room))

	return nil
}

func (s *LocalStore) ListAgentDispatches(ctx context.Context, roomName livekit.RoomName) ([]*livekit.AgentDispatch, error) {
	s.lock.Lock()
	defer s.lock.Unlock()

	ds := maps.Values(s.agentDispatches)
	js := maps.Values(s.agentJobs)

	m := make(map[string]*livekit.AgentDispatch)
	for _, d := range s.agentDispatches {
		m[d.Id] = d
	}

	for _, j := range js {
		d := m[j.DispatchId]
		if d != nil {
			d.State.Jobs = append(d.State.Jobs, j)
		}
	}

	return ds, nil
}

func (s *LocalStore) StoreAgentJob(ctx context.Context, job *livekit.Job) error {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.agentJobs[livekit.RoomName(job.Room.Name)] = job

	return nil
}

func (s *LocalStore) DeleteAgentJob(ctx context.Context, job *livekit.Job) error {
	s.lock.Lock()
	defer s.lock.Unlock()

	delete(s.agentJobs, livekit.RoomName(job.Room.Name))

	return nil
}

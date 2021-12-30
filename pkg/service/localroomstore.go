package service

import (
	"context"
	"sync"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/thoas/go-funk"
)

// encapsulates CRUD operations for room settings
type LocalRoomStore struct {
	// map of roomName => room
	rooms map[livekit.RoomName]*livekit.Room
	// map of roomName => { identity: participant }
	participants map[livekit.RoomName]map[livekit.ParticipantIdentity]*livekit.ParticipantInfo
	lock         sync.RWMutex
	globalLock   sync.Mutex
}

func NewLocalRoomStore() *LocalRoomStore {
	return &LocalRoomStore{
		rooms:        make(map[livekit.RoomName]*livekit.Room),
		participants: make(map[livekit.RoomName]map[livekit.ParticipantIdentity]*livekit.ParticipantInfo),
		lock:         sync.RWMutex{},
	}
}

func (p *LocalRoomStore) StoreRoom(_ context.Context, room *livekit.Room) error {
	if room.CreationTime == 0 {
		room.CreationTime = time.Now().Unix()
	}
	p.lock.Lock()
	p.rooms[room.Name] = room
	p.lock.Unlock()
	return nil
}

func (p *LocalRoomStore) LoadRoom(_ context.Context, name livekit.RoomName) (*livekit.Room, error) {
	p.lock.RLock()
	defer p.lock.RUnlock()

	room := p.rooms[name]
	if room == nil {
		return nil, ErrRoomNotFound
	}
	return room, nil
}

func (p *LocalRoomStore) ListRooms(_ context.Context, names []livekit.RoomName) ([]*livekit.Room, error) {
	p.lock.RLock()
	defer p.lock.RUnlock()
	rooms := make([]*livekit.Room, 0, len(p.rooms))
	for _, r := range p.rooms {
		if names == nil || funk.Contains(names, r.Name) {
			rooms = append(rooms, r)
		}
	}
	return rooms, nil
}

func (p *LocalRoomStore) DeleteRoom(ctx context.Context, name livekit.RoomName) error {
	room, err := p.LoadRoom(ctx, name)
	if err == ErrRoomNotFound {
		return nil
	} else if err != nil {
		return err
	}

	p.lock.Lock()
	defer p.lock.Unlock()

	delete(p.participants, room.Name)
	delete(p.rooms, room.Name)
	return nil
}

func (p *LocalRoomStore) LockRoom(_ context.Context, _ livekit.RoomName, _ time.Duration) (string, error) {
	// local rooms lock & unlock globally
	p.globalLock.Lock()
	return "", nil
}

func (p *LocalRoomStore) UnlockRoom(_ context.Context, _ livekit.RoomName, _ string) error {
	p.globalLock.Unlock()
	return nil
}

func (p *LocalRoomStore) StoreParticipant(_ context.Context, roomName livekit.RoomName, participant *livekit.ParticipantInfo) error {
	p.lock.Lock()
	defer p.lock.Unlock()
	roomParticipants := p.participants[roomName]
	if roomParticipants == nil {
		roomParticipants = make(map[livekit.ParticipantIdentity]*livekit.ParticipantInfo)
		p.participants[roomName] = roomParticipants
	}
	roomParticipants[participant.Identity] = participant
	return nil
}

func (p *LocalRoomStore) LoadParticipant(_ context.Context, roomName, identity livekit.ParticipantIdentity) (*livekit.ParticipantInfo, error) {
	p.lock.RLock()
	defer p.lock.RUnlock()

	roomParticipants := p.participants[roomName]
	if roomParticipants == nil {
		return nil, ErrParticipantNotFound
	}
	participant := roomParticipants[identity]
	if participant == nil {
		return nil, ErrParticipantNotFound
	}
	return participant, nil
}

func (p *LocalRoomStore) ListParticipants(_ context.Context, roomName livekit.RoomName) ([]*livekit.ParticipantInfo, error) {
	p.lock.RLock()
	defer p.lock.RUnlock()

	roomParticipants := p.participants[roomName]
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

func (p *LocalRoomStore) DeleteParticipant(_ context.Context, roomName, identity livekit.ParticipantIdentity) error {
	p.lock.Lock()
	defer p.lock.Unlock()

	roomParticipants := p.participants[roomName]
	if roomParticipants != nil {
		delete(roomParticipants, identity)
	}
	return nil
}

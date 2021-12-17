package service

import (
	"context"
	"sync"
	"time"

	"github.com/livekit/protocol/livekit"
)

// encapsulates CRUD operations for room settings
type LocalRoomStore struct {
	// map of roomName => room
	rooms map[string]*livekit.Room
	// map of roomName => { identity: participant }
	participants map[string]map[string]*livekit.ParticipantInfo
	lock         sync.RWMutex
	globalLock   sync.Mutex
}

func NewLocalRoomStore() *LocalRoomStore {
	return &LocalRoomStore{
		rooms:        make(map[string]*livekit.Room),
		participants: make(map[string]map[string]*livekit.ParticipantInfo),
		lock:         sync.RWMutex{},
	}
}

func (p *LocalRoomStore) StoreRoom(ctx context.Context, room *livekit.Room) error {
	if room.CreationTime == 0 {
		room.CreationTime = time.Now().Unix()
	}
	p.lock.Lock()
	p.rooms[room.Name] = room
	p.lock.Unlock()
	return nil
}

func (p *LocalRoomStore) LoadRoom(ctx context.Context, name string) (*livekit.Room, error) {
	p.lock.RLock()
	defer p.lock.RUnlock()

	room := p.rooms[name]
	if room == nil {
		return nil, ErrRoomNotFound
	}
	return room, nil
}

func (p *LocalRoomStore) ListRooms(ctx context.Context) ([]*livekit.Room, error) {
	p.lock.RLock()
	defer p.lock.RUnlock()
	rooms := make([]*livekit.Room, 0, len(p.rooms))
	for _, r := range p.rooms {
		rooms = append(rooms, r)
	}
	return rooms, nil
}

func (p *LocalRoomStore) DeleteRoom(ctx context.Context, name string) error {
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

func (p *LocalRoomStore) LockRoom(ctx context.Context, name string, duration time.Duration) (string, error) {
	// local rooms lock & unlock globally
	p.globalLock.Lock()
	return "", nil
}

func (p *LocalRoomStore) UnlockRoom(ctx context.Context, name string, uid string) error {
	p.globalLock.Unlock()
	return nil
}

func (p *LocalRoomStore) StoreParticipant(ctx context.Context, roomName string, participant *livekit.ParticipantInfo) error {
	p.lock.Lock()
	defer p.lock.Unlock()
	roomParticipants := p.participants[roomName]
	if roomParticipants == nil {
		roomParticipants = make(map[string]*livekit.ParticipantInfo)
		p.participants[roomName] = roomParticipants
	}
	roomParticipants[participant.Identity] = participant
	return nil
}

func (p *LocalRoomStore) LoadParticipant(ctx context.Context, roomName, identity string) (*livekit.ParticipantInfo, error) {
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

func (p *LocalRoomStore) ListParticipants(ctx context.Context, roomName string) ([]*livekit.ParticipantInfo, error) {
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

func (p *LocalRoomStore) DeleteParticipant(ctx context.Context, roomName, identity string) error {
	p.lock.Lock()
	defer p.lock.Unlock()

	roomParticipants := p.participants[roomName]
	if roomParticipants != nil {
		delete(roomParticipants, identity)
	}
	return nil
}

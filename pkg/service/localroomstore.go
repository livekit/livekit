package service

import (
	"sync"
	"time"

	livekit "github.com/livekit/protocol/proto"
)

// encapsulates CRUD operations for room settings
type LocalRoomStore struct {
	// map of roomId => room
	rooms map[string]*livekit.Room
	// map of roomName => roomId
	roomIds map[string]string
	// map of roomName => { identity: participant }
	participants map[string]map[string]*livekit.ParticipantInfo
	lock         sync.RWMutex
	globalLock   sync.Mutex
}

func NewLocalRoomStore() *LocalRoomStore {
	return &LocalRoomStore{
		rooms:        make(map[string]*livekit.Room),
		roomIds:      make(map[string]string),
		participants: make(map[string]map[string]*livekit.ParticipantInfo),
		lock:         sync.RWMutex{},
	}
}

func (p *LocalRoomStore) StoreRoom(room *livekit.Room) error {
	if room.CreationTime == 0 {
		room.CreationTime = time.Now().Unix()
	}
	p.lock.Lock()
	p.rooms[room.Sid] = room
	p.roomIds[room.Name] = room.Sid
	p.lock.Unlock()
	return nil
}

func (p *LocalRoomStore) LoadRoom(idOrName string) (*livekit.Room, error) {
	p.lock.RLock()
	defer p.lock.RUnlock()
	// see if it's an id or name
	if p.rooms[idOrName] == nil {
		idOrName = p.roomIds[idOrName]
	}

	room := p.rooms[idOrName]
	if room == nil {
		return nil, ErrRoomNotFound
	}
	return room, nil
}

func (p *LocalRoomStore) ListRooms() ([]*livekit.Room, error) {
	p.lock.RLock()
	defer p.lock.RUnlock()
	rooms := make([]*livekit.Room, 0, len(p.rooms))
	for _, r := range p.rooms {
		rooms = append(rooms, r)
	}
	return rooms, nil
}

func (p *LocalRoomStore) DeleteRoom(idOrName string) error {
	room, err := p.LoadRoom(idOrName)
	if err == ErrRoomNotFound {
		return nil
	} else if err != nil {
		return err
	}

	p.lock.Lock()
	defer p.lock.Unlock()

	delete(p.participants, room.Name)
	delete(p.roomIds, room.Name)
	delete(p.rooms, room.Sid)
	return nil
}

func (p *LocalRoomStore) LockRoom(name string, duration time.Duration) (string, error) {
	// local rooms lock & unlock globally
	p.globalLock.Lock()
	return "", nil
}

func (p *LocalRoomStore) UnlockRoom(name string, uid string) error {
	p.globalLock.Unlock()
	return nil
}

func (p *LocalRoomStore) PersistParticipant(roomName string, participant *livekit.ParticipantInfo) error {
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

func (p *LocalRoomStore) GetParticipant(roomName, identity string) (*livekit.ParticipantInfo, error) {
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

func (p *LocalRoomStore) ListParticipants(roomName string) ([]*livekit.ParticipantInfo, error) {
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

func (p *LocalRoomStore) DeleteParticipant(roomName, identity string) error {
	p.lock.Lock()
	defer p.lock.Unlock()

	roomParticipants := p.participants[roomName]
	if roomParticipants != nil {
		delete(roomParticipants, identity)
	}
	return nil
}

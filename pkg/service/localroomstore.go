package service

import (
	"sync"

	"github.com/thoas/go-funk"

	"github.com/livekit/livekit-server/proto/livekit"
)

// encapsulates CRUD operations for room settings
type LocalRoomStore struct {
	// map of roomId => room
	rooms map[string]*livekit.Room
	// map of roomName => roomId
	roomIds map[string]string
	lock    sync.RWMutex
}

func NewLocalRoomStore() *LocalRoomStore {
	return &LocalRoomStore{
		rooms:   make(map[string]*livekit.Room),
		roomIds: make(map[string]string),
		lock:    sync.RWMutex{},
	}
}

func (p *LocalRoomStore) CreateRoom(room *livekit.Room) error {
	p.lock.Lock()
	p.rooms[room.Sid] = room
	p.roomIds[room.Name] = room.Sid
	p.lock.Unlock()
	return nil
}

func (p *LocalRoomStore) GetRoom(idOrName string) (*livekit.Room, error) {
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
	return funk.Values(p.rooms).([]*livekit.Room), nil
}

func (p *LocalRoomStore) DeleteRoom(idOrName string) error {
	room, err := p.GetRoom(idOrName)
	if err == ErrRoomNotFound {
		return nil
	} else if err != nil {
		return err
	}

	p.lock.Lock()
	defer p.lock.Unlock()

	delete(p.rooms, room.Sid)
	delete(p.roomIds, room.Name)
	return nil
}

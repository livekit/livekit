package service

import (
	"context"
	"log"
	"sync"
	"time"

	p2p_database "github.com/dTelecom/p2p-realtime-database"
	"github.com/thoas/go-funk"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/p2p"
)

// encapsulates CRUD operations for room settings
type LocalStore struct {
	currentNodeId     livekit.NodeID
	p2pDatabaseConfig p2p_database.Config

	// map of roomName => room
	rooms        map[livekit.RoomName]*livekit.Room
	roomInternal map[livekit.RoomName]*livekit.RoomInternal
	// map of roomName => { identity: participant }
	participants map[livekit.RoomName]map[livekit.ParticipantIdentity]*livekit.ParticipantInfo

	roomCommunicators map[livekit.RoomName]*p2p.RoomCommunicatorImpl

	lock       sync.RWMutex
	globalLock sync.Mutex
}

func NewLocalStore(currentNodeId livekit.NodeID, mainDatabase p2p_database.Config) *LocalStore {
	return &LocalStore{
		currentNodeId:     currentNodeId,
		p2pDatabaseConfig: mainDatabase,
		rooms:             make(map[livekit.RoomName]*livekit.Room),
		roomInternal:      make(map[livekit.RoomName]*livekit.RoomInternal),
		participants:      make(map[livekit.RoomName]map[livekit.ParticipantIdentity]*livekit.ParticipantInfo),
		roomCommunicators: make(map[livekit.RoomName]*p2p.RoomCommunicatorImpl),
		lock:              sync.RWMutex{},
	}
}

func (s *LocalStore) StoreRoom(_ context.Context, room *livekit.Room, internal *livekit.RoomInternal) error {
	log.Println("Calling localstore.StoreRoom")
	if room.CreationTime == 0 {
		room.CreationTime = time.Now().Unix()
	}

	roomName := livekit.RoomName(room.Name)

	s.lock.Lock()
	s.rooms[roomName] = room
	s.roomInternal[roomName] = internal
	if _, ok := s.roomCommunicators[roomName]; !ok {
		s.roomCommunicators[roomName] = p2p.NewRoomCommunicatorImpl(room, s.p2pDatabaseConfig)
	}
	s.lock.Unlock()

	return nil
}

func (s *LocalStore) LoadRoom(_ context.Context, roomName livekit.RoomName, includeInternal bool) (*livekit.Room, *livekit.RoomInternal, p2p.RoomCommunicator, error) {
	s.lock.RLock()
	defer s.lock.RUnlock()

	room := s.rooms[roomName]
	if room == nil {
		return nil, nil, nil, ErrRoomNotFound
	}

	var internal *livekit.RoomInternal
	if includeInternal {
		internal = s.roomInternal[roomName]
	}

	roomCommunicator := s.roomCommunicators[roomName]

	return room, internal, roomCommunicator, nil
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
	log.Println("Calling localstore.DeleteRoom")

	room, _, _, err := s.LoadRoom(ctx, roomName, false)
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

	db, exists := s.roomCommunicators[livekit.RoomName(room.Name)]
	if exists {
		db.Close()
	}

	delete(s.roomCommunicators, livekit.RoomName(room.Name))

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

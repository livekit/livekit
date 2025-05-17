package service

import (
	"context"
	"sync"
	"time"

	p2p_database "github.com/dTelecom/p2p-database"
	"github.com/pkg/errors"

	"github.com/thoas/go-funk"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/p2p"
)

// encapsulates CRUD operations for room settings
type LocalStore struct {
	currentNodeId livekit.NodeID
	db            *p2p_database.DB

	// map of roomKey => room
	rooms        map[livekit.RoomKey]*livekit.Room
	roomInternal map[livekit.RoomKey]*livekit.RoomInternal
	// map of roomKey => { identity: participant }
	participants map[livekit.RoomKey]map[livekit.ParticipantIdentity]*livekit.ParticipantInfo

	roomCommunicators map[livekit.RoomKey]*p2p.RoomCommunicatorImpl
	lock              sync.RWMutex
	globalLock        sync.Mutex
}

func NewLocalStore(
	currentNodeId livekit.NodeID,
	db *p2p_database.DB,
) *LocalStore {
	return &LocalStore{
		currentNodeId: currentNodeId,
		db:            db,

		rooms:             make(map[livekit.RoomKey]*livekit.Room),
		roomInternal:      make(map[livekit.RoomKey]*livekit.RoomInternal),
		participants:      make(map[livekit.RoomKey]map[livekit.ParticipantIdentity]*livekit.ParticipantInfo),
		roomCommunicators: make(map[livekit.RoomKey]*p2p.RoomCommunicatorImpl),

		lock: sync.RWMutex{},
	}
}

func (s *LocalStore) StoreRoom(_ context.Context, room *livekit.Room, roomKey livekit.RoomKey, internal *livekit.RoomInternal) error {
	if room.CreationTime == 0 {
		room.CreationTime = time.Now().Unix()
	}

	s.lock.Lock()
	s.rooms[roomKey] = room
	s.roomInternal[roomKey] = internal
	if _, ok := s.roomCommunicators[roomKey]; !ok {
		if roomCommunicator, err := p2p.NewRoomCommunicatorImpl(room, s.db); err != nil {
			return errors.Wrap(err, "cannot create room communicator")
		} else {
			s.roomCommunicators[roomKey] = roomCommunicator
			logger.Debugw("New room communicator has been created")
		}
	} else {
		logger.Debugw("Room communicator already exists")
	}
	s.lock.Unlock()

	return nil
}

func (s *LocalStore) LoadRoom(_ context.Context, roomKey livekit.RoomKey, includeInternal bool) (*livekit.Room, *livekit.RoomInternal, p2p.RoomCommunicator, error) {
	s.lock.RLock()
	defer s.lock.RUnlock()

	room := s.rooms[roomKey]
	if room == nil {
		return nil, nil, nil, ErrRoomNotFound
	}

	var internal *livekit.RoomInternal
	if includeInternal {
		internal = s.roomInternal[roomKey]
	}

	roomCommunicator := s.roomCommunicators[roomKey]

	return room, internal, roomCommunicator, nil
}

func (s *LocalStore) ListRooms(_ context.Context, roomKeys []livekit.RoomKey) ([]*livekit.Room, error) {
	s.lock.RLock()
	defer s.lock.RUnlock()
	rooms := make([]*livekit.Room, 0, len(s.rooms))
	for k, r := range s.rooms {
		if roomKeys == nil || funk.Contains(roomKeys, k) {
			rooms = append(rooms, r)
		}
	}
	return rooms, nil
}

func (s *LocalStore) DeleteRoom(ctx context.Context, roomKey livekit.RoomKey) error {
	_, _, _, err := s.LoadRoom(ctx, roomKey, false)
	if err == ErrRoomNotFound {
		return nil
	} else if err != nil {
		return err
	}

	s.lock.Lock()
	defer s.lock.Unlock()

	delete(s.participants, roomKey)
	delete(s.rooms, roomKey)
	delete(s.roomInternal, roomKey)

	db, exists := s.roomCommunicators[roomKey]
	if exists {
		db.Close()
		delete(s.roomCommunicators, roomKey)
	}

	return nil
}

func (s *LocalStore) LockRoom(_ context.Context, _ livekit.RoomKey, _ time.Duration) (string, error) {
	// local rooms lock & unlock globally
	s.globalLock.Lock()
	return "", nil
}

func (s *LocalStore) UnlockRoom(_ context.Context, _ livekit.RoomKey, _ string) error {
	s.globalLock.Unlock()
	return nil
}

func (s *LocalStore) StoreParticipant(ctx context.Context, roomKey livekit.RoomKey, participant *livekit.ParticipantInfo) error {
	s.lock.Lock()
	defer s.lock.Unlock()
	roomParticipants := s.participants[roomKey]
	if roomParticipants == nil {
		roomParticipants = make(map[livekit.ParticipantIdentity]*livekit.ParticipantInfo)
		s.participants[roomKey] = roomParticipants
	}

	roomParticipants[livekit.ParticipantIdentity(participant.Identity)] = participant
	return nil
}

func (s *LocalStore) LoadParticipant(_ context.Context, roomKey livekit.RoomKey, identity livekit.ParticipantIdentity) (*livekit.ParticipantInfo, error) {
	s.lock.RLock()
	defer s.lock.RUnlock()

	roomParticipants := s.participants[roomKey]
	if roomParticipants == nil {
		return nil, ErrParticipantNotFound
	}
	participant := roomParticipants[identity]
	if participant == nil {
		return nil, ErrParticipantNotFound
	}
	return participant, nil
}

func (s *LocalStore) ListParticipants(_ context.Context, roomKey livekit.RoomKey) ([]*livekit.ParticipantInfo, error) {
	s.lock.RLock()
	defer s.lock.RUnlock()

	roomParticipants := s.participants[roomKey]
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

func (s *LocalStore) DeleteParticipant(ctx context.Context, roomKey livekit.RoomKey, identity livekit.ParticipantIdentity) error {
	s.lock.Lock()
	defer s.lock.Unlock()

	roomParticipants := s.participants[roomKey]
	if roomParticipants != nil {
		_, participantExists := roomParticipants[identity]
		if participantExists {
			delete(roomParticipants, identity)
		}
	}
	return nil
}

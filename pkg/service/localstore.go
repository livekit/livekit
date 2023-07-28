package service

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/protocol/logger"
	"github.com/pkg/errors"

	p2p_database "github.com/dTelecom/p2p-realtime-database"
	"github.com/thoas/go-funk"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/p2p"
)

// encapsulates CRUD operations for room settings
type LocalStore struct {
	currentNodeId            livekit.NodeID
	p2pDatabaseConfig        p2p_database.Config
	clientParticipantCounter *ParticipantCounter
	nodeProvider             *NodeProvider
	mainDatabase             *p2p_database.DB

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
	mainDatabaseConfig p2p_database.Config,
	participantCounter *ParticipantCounter,
	mainDatabase *p2p_database.DB,
	nodeProvider *NodeProvider,
) *LocalStore {
	return &LocalStore{
		currentNodeId:            currentNodeId,
		p2pDatabaseConfig:        mainDatabaseConfig,
		clientParticipantCounter: participantCounter,
		nodeProvider:             nodeProvider,
		mainDatabase:             mainDatabase,

		rooms:             make(map[livekit.RoomKey]*livekit.Room),
		roomInternal:      make(map[livekit.RoomKey]*livekit.RoomInternal),
		participants:      make(map[livekit.RoomKey]map[livekit.ParticipantIdentity]*livekit.ParticipantInfo),
		roomCommunicators: make(map[livekit.RoomKey]*p2p.RoomCommunicatorImpl),

		lock: sync.RWMutex{},
	}
}

func (s *LocalStore) StoreRoom(_ context.Context, room *livekit.Room, roomKey livekit.RoomKey, internal *livekit.RoomInternal) error {
	log.Println("Calling localstore.StoreRoom")
	if room.CreationTime == 0 {
		room.CreationTime = time.Now().Unix()
	}

	s.lock.Lock()
	s.rooms[roomKey] = room
	s.roomInternal[roomKey] = internal
	if _, ok := s.roomCommunicators[roomKey]; !ok {
		cfg := s.p2pDatabaseConfig
		s.roomCommunicators[roomKey] = p2p.NewRoomCommunicatorImpl(room, cfg)
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
	log.Println("Calling localstore.DeleteRoom")

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
	}

	delete(s.roomCommunicators, roomKey)

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

func (s *LocalStore) StoreParticipant(ctx context.Context, roomKey livekit.RoomKey, participant *livekit.ParticipantInfo, relayed bool) error {
	log.Println("Calling localstore.StoreParticipant")

	participant.Relayed = relayed
	_, apiKey, err := utils.ParseRoomKey(roomKey)
	if err != nil {
		return errors.Wrap(err, "parse room key")
	}

	s.lock.Lock()
	defer s.lock.Unlock()
	roomParticipants := s.participants[roomKey]
	if roomParticipants == nil {
		roomParticipants = make(map[livekit.ParticipantIdentity]*livekit.ParticipantInfo)
		s.participants[roomKey] = roomParticipants
	}

	if participant.Relayed == false {
		_, participantExists := roomParticipants[livekit.ParticipantIdentity(participant.Identity)]
		if !participantExists {
			err = s.nodeProvider.IncrementParticipants(ctx, s.mainDatabase.GetHost().ID().String())
			if err != nil {
				logger.Errorw("increment participants count", err)
			}

			err = s.clientParticipantCounter.Increment(ctx, string(apiKey), participant.Identity)
			if err != nil {
				logger.Errorw("cannot increment participant count", err)
			}
		}
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
	log.Println("Calling localstore.DeleteParticipant")

	s.lock.Lock()
	defer s.lock.Unlock()

	_, apiKey, err := utils.ParseRoomKey(roomKey)
	if err != nil {
		return errors.Wrap(err, "parse room key")
	}

	roomParticipants := s.participants[roomKey]
	if roomParticipants != nil {
		participant, participantExists := roomParticipants[identity]
		if participantExists {
			delete(roomParticipants, identity)
			if participant.Relayed == false {
				err := s.clientParticipantCounter.Decrement(ctx, string(apiKey), string(identity))
				if err != nil {
					logger.Errorw("cannot decrement participant count", err)
				}
				err = s.nodeProvider.DecrementParticipants(ctx, s.mainDatabase.GetHost().ID().String())
				if err != nil {
					logger.Errorw("decrement participants count: %s", err)
				}
			}
		}
	}
	return nil
}

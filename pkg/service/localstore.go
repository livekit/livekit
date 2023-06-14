package service

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	p2p_database "github.com/dTelecom/p2p-realtime-database"
	logging "github.com/ipfs/go-log/v2"
	"github.com/pkg/errors"

	"github.com/thoas/go-funk"

	"github.com/livekit/protocol/livekit"
)

const (
	prefixPeerKey = "node_"
	pingMessage   = "ping"
	pongMessage   = "pong"
)

type RoomDatabase struct {
	p2p         *p2p_database.DB
	syncedPeers sync.Map
	ctx         context.Context
	cancel      context.CancelFunc
}

// encapsulates CRUD operations for room settings
type LocalStore struct {
	currentNodeId     livekit.NodeID
	p2pDatabaseConfig p2p_database.Config

	// map of roomName => room
	rooms        map[livekit.RoomName]*livekit.Room
	roomInternal map[livekit.RoomName]*livekit.RoomInternal
	// map of roomName => { identity: participant }
	participants map[livekit.RoomName]map[livekit.ParticipantIdentity]*livekit.ParticipantInfo

	databases              map[livekit.RoomName]*RoomDatabase
	roomCommunicationsInit map[livekit.RoomName]*sync.Once

	lock       sync.RWMutex
	globalLock sync.Mutex
}

func NewLocalStore(currentNodeId livekit.NodeID, mainDatabase p2p_database.Config) *LocalStore {
	return &LocalStore{
		currentNodeId:          currentNodeId,
		p2pDatabaseConfig:      mainDatabase,
		rooms:                  make(map[livekit.RoomName]*livekit.Room),
		roomInternal:           make(map[livekit.RoomName]*livekit.RoomInternal),
		participants:           make(map[livekit.RoomName]map[livekit.ParticipantIdentity]*livekit.ParticipantInfo),
		databases:              make(map[livekit.RoomName]*RoomDatabase),
		roomCommunicationsInit: map[livekit.RoomName]*sync.Once{},
		lock:                   sync.RWMutex{},
	}
}

func (s *LocalStore) getOrCreateDatabase(room *livekit.Room, cfg p2p_database.Config) (*RoomDatabase, error) {
	s.lock.Lock()
	defer s.lock.Unlock()

	roomDb, ok := s.databases[livekit.RoomName(room.Name)]
	if ok {
		return roomDb, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	cfg.DatabaseName = "livekit_room_" + room.Name

	_ = logging.SetLogLevel("*", "error")
	db, err := p2p_database.Connect(ctx, cfg, logging.Logger("db_livekit_room_"+room.Name))
	if err != nil {
		cancel()
		return nil, errors.Wrapf(err, "create or connect livekit database room %s", room.Name)
	}

	roomDb = &RoomDatabase{
		p2p:         db,
		ctx:         ctx,
		cancel:      cancel,
		syncedPeers: sync.Map{},
	}
	s.databases[livekit.RoomName(room.Name)] = roomDb

	return roomDb, nil
}

func (s *LocalStore) StoreRoom(ctx context.Context, room *livekit.Room, internal *livekit.RoomInternal) error {
	if room.CreationTime == 0 {
		room.CreationTime = time.Now().Unix()
	}

	roomName := livekit.RoomName(room.Name)
	s.lock.Lock()
	_, ok := s.roomCommunicationsInit[roomName]
	if !ok {
		s.roomCommunicationsInit[roomName] = &sync.Once{}
	}
	s.lock.Unlock()

	var err error
	once := s.roomCommunicationsInit[roomName]
	once.Do(func() {
		var (
			db           *p2p_database.DB
			roomDatabase *RoomDatabase
			nc           *NodeCommunication
		)

		cfg := s.p2pDatabaseConfig
		cfg.NewKeyCallback = func(k string) {
			k = strings.TrimPrefix(k, "/")
			if !strings.HasPrefix(k, prefixPeerKey) {
				return
			}
			peerId := strings.TrimPrefix(k, prefixPeerKey)
			if peerId == db.GetHost().ID().String() {
				return
			}

			_, alreadySynced := roomDatabase.syncedPeers.Load(peerId)
			if alreadySynced {
				return
			}

			_, err = nc.SendAsyncMessageToPeerId(ctx, peerId, pingMessage)
			if err != nil {
				log.Fatalf("cannot send ping message for node %s in db %s: %s", peerId, room.Name, err)
			} else {
				log.Printf("%s send ping message to node %s in db %s", db.GetHost().ID(), peerId, room.Name)
				roomDatabase.syncedPeers.Store(peerId, peerId)
			}
		}

		roomDatabase, err = s.getOrCreateDatabase(room, cfg)
		if err != nil {
			err = errors.Wrapf(err, "error init database for room %s", room.Name)
			return
		}

		db = roomDatabase.p2p
		nc = NewNodeCommunication(db)

		nc.Setup(context.Background(), func(e p2p_database.Event) {
			if e.Message != pingMessage {
				return
			}
			msgId, err := nc.SendAsyncMessageToPeerId(ctx, e.FromPeerId, pongMessage)
			if err != nil {
				log.Fatalf("%s cannot send pong message database room %s to peer msgId %s", db.GetHost().ID(), db.Name, e.FromPeerId)
				return
			}
			log.Printf("%s send pong message %s db %s to peer %s", db.GetHost().ID(), msgId, db.Name, e.FromPeerId)
		})

		err = db.Set(ctx, prefixPeerKey+db.GetHost().ID().String(), time.Now().String())
		if err != nil {
			err = errors.Wrapf(err, "cannot set node id to db %s", room.Name)
			return
		}
	})

	if err != nil {
		return err
	}

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

	db, exists := s.databases[livekit.RoomName(room.Name)]
	if exists {
		k := prefixPeerKey + db.p2p.GetHost().ID().String()
		err := db.p2p.Remove(ctx, k)
		if err != nil {
			log.Printf("try remove key %s for room db %s error: %s", k, room.Name, err)
		}
		db.cancel()
	}

	delete(s.databases, livekit.RoomName(room.Name))
	delete(s.roomCommunicationsInit, livekit.RoomName(room.Name))

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

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

var syncedPeers = sync.Map{}

// encapsulates CRUD operations for room settings
type LocalStore struct {
	currentNodeId     livekit.NodeID
	p2pDatabaseConfig p2p_database.Config

	// map of roomName => room
	rooms        map[livekit.RoomName]*livekit.Room
	roomInternal map[livekit.RoomName]*livekit.RoomInternal
	// map of roomName => { identity: participant }
	participants map[livekit.RoomName]map[livekit.ParticipantIdentity]*livekit.ParticipantInfo

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
		lock:              sync.RWMutex{},
	}
}

func (s *LocalStore) StoreRoom(ctx context.Context, room *livekit.Room, internal *livekit.RoomInternal) error {
	if room.CreationTime == 0 {
		room.CreationTime = time.Now().Unix()
	}
	roomName := livekit.RoomName(room.Name)

	cfg := s.p2pDatabaseConfig
	cfg.DatabaseName = "livekit_room_" + room.Name
	db, err := p2p_database.Connect(context.Background(), cfg, logging.Logger("db_livekit_room_"+room.Name))
	if err != nil {
		return errors.Wrapf(err, "create or connect livekit database room %s", room.Name)
	}

	nc := NewNodeCommunication(db)
	go func() {
		responsesChannel, err := nc.ListenIncomingMessages(ctx)
		if err != nil {
			log.Fatalf("cannot listen incoming messsages database room %s", room.Name)
		}

		msg := <-responsesChannel
		log.Printf("got message %s from node %s from database room %s: %s", msg.ID, msg.FromPeerId, room.Name, msg.Message)

		if msg.Message == pingMessage {
			msgId, err := nc.SendAsyncMessageToPeerId(ctx, msg.FromPeerId, pongMessage)
			if err != nil {
				log.Fatalf("cannot send pong message database room %s to peer msgId %s", room.Name, msg.FromPeerId)
			}
			log.Printf("send pong message %s db %s to peer %s", msgId, db.Name, msg.FromPeerId)
		}
	}()

	err = db.Set(ctx, prefixPeerKey+db.GetHost().ID().String(), time.Now().String())
	if err != nil {
		return errors.Wrapf(err, "cannot set node id to db %s", room.Name)
	}

	go func() {
		for {
			log.Printf("search new peers in room %s", room.Name)

			keys, err := db.List(ctx)
			if err != nil {
				log.Fatalf("get connected nodes for db %s: %s", room.Name, err)
			}
			for _, k := range keys {
				if !strings.HasPrefix(k, prefixPeerKey) {
					continue
				}
				peerId := strings.TrimPrefix(k, prefixPeerKey)

				_, alreadySynced := syncedPeers.Load(peerId)
				if alreadySynced {
					continue
				}

				msgId, err := nc.SendAsyncMessageToPeerId(ctx, peerId, pingMessage)
				if err != nil {
					log.Fatalf("cannot send ping message for node %s in db %s: %s", peerId, room.Name, err)
				}

				log.Printf("send ping message to node %s in db %s", peerId, room.Name)
				syncedPeers.Store(peerId, msgId)
			}

			time.Sleep(500 * time.Millisecond)
		}
	}()

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

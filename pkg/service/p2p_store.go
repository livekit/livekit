package service

import (
	"context"
	p2p_database "github.com/dTelecom/p2p-realtime-database"
	logging "github.com/ipfs/go-log/v2"
	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/protocol/livekit"
	"github.com/pkg/errors"
	"google.golang.org/protobuf/proto"
	"strconv"
	"strings"
	"time"
)

var (
	keyRoomPrefix         = "rooms_"
	keyRoomIntervalPrefix = "rooms_interval_"
	keyLockRoomPrefix     = "room_lock_"

	keyPrefixParticipant = "participant_"
)

type P2pStore struct {
	db *p2p_database.DB
}

func NewP2pStore(ctx context.Context, id livekit.NodeID, conf *config.Config) (*P2pStore, error) {
	db, err := p2p_database.Connect(ctx, p2p_database.Config{
		PeerListenPort:          conf.Ethereum.P2pNodePort,
		EthereumNetworkHost:     conf.Ethereum.NetworkHost,
		EthereumNetworkKey:      conf.Ethereum.NetworkKey,
		EthereumContractAddress: conf.Ethereum.ContractAddress,
		WalletPrivateKey:        conf.Ethereum.WalletPrivateKey,
		DatabaseName:            "livekit_" + string(id),
	}, logging.Logger("db"))

	if err != nil {
		return nil, errors.Wrap(err, "connect db")
	}

	return &P2pStore{db: db}, nil
}

func (p *P2pStore) StoreRoom(ctx context.Context, room *livekit.Room, internal *livekit.RoomInternal) error {
	marshaledRoom, err := proto.Marshal(room)
	if err != nil {
		return errors.Wrap(err, "cannot marshal room")
	}

	marshaledInterval, err := proto.Marshal(internal)
	if err != nil {
		return errors.Wrap(err, "cannot marshal internal")
	}

	err = p.db.Set(ctx, keyRoomPrefix+room.Name, string(marshaledRoom))
	if err != nil {
		return errors.Wrap(err, "cannot save marshaled room")
	}

	err = p.db.Set(ctx, keyRoomIntervalPrefix+room.Name, string(marshaledInterval))
	if err != nil {
		return errors.Wrap(err, "cannot save marshaled room")
	}

	return nil
}

func (p *P2pStore) LoadRoom(ctx context.Context, roomName livekit.RoomName, includeInternal bool) (*livekit.Room, *livekit.RoomInternal, error) {
	roomMarshaled, err := p.db.Get(ctx, keyRoomPrefix+string(roomName))
	switch {
	case errors.Is(err, p2p_database.ErrKeyNotFound):
		return nil, nil, ErrRoomNotFound
	case err != nil:
		return nil, nil, errors.Wrap(err, "fetch room from db")
	}

	room := &livekit.Room{}
	err = proto.Unmarshal([]byte(roomMarshaled), room)
	if err != nil {
		return nil, nil, errors.Wrap(err, "cannot unmarshal loaded room")
	}

	interval := &livekit.RoomInternal{}
	if includeInternal {
		resInterval, err := p.db.Get(ctx, keyRoomIntervalPrefix+string(roomName))
		if err != nil {
			return nil, nil, errors.Wrap(err, "fetch room interval from db")
		}
		err = proto.Unmarshal([]byte(resInterval), interval)
		if err != nil {
			return nil, nil, errors.Wrap(err, "cannot unmarshal loaded room interval")
		}
	}

	return room, interval, nil
}

func (p *P2pStore) ListRooms(ctx context.Context, roomNames []livekit.RoomName) ([]*livekit.Room, error) {
	keys, err := p.db.List(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "db list keys")
	}

	var (
		roomNamesKeys []string
		res           = make([]*livekit.Room, 0, len(roomNamesKeys))
	)

	for _, k := range keys {
		for _, roomName := range roomNames {
			if strings.TrimLeft(k, keyRoomPrefix) != string(roomName) {
				continue
			}
			room, _, err := p.LoadRoom(ctx, roomName, false)
			if err != nil {
				return nil, errors.Wrap(err, "fetch room from list")
			}
			res = append(res, room)
		}
	}

	return res, nil
}

func (p *P2pStore) LockRoom(ctx context.Context, roomName livekit.RoomName, duration time.Duration) (string, error) {
	lockValue := int(time.Now().Add(duration).UnixNano())

	k := keyLockRoomPrefix + roomNameKey(roomName)

	actualLockValueString, err := p.db.Get(ctx, k)
	if err == nil {
		actualLockValue, err := strconv.Atoi(actualLockValueString)
		if err != nil {
			return "", errors.Wrap(err, "convert actual lock value to int")
		}
		if actualLockValue > lockValue {
			time.Sleep(time.Duration(actualLockValue-lockValue) * time.Nanosecond)
		}
	}

	err = p.db.Set(ctx, k, strconv.Itoa(lockValue))
	if err != nil {
		return "", errors.Wrap(err, "cannot save marshaled room")
	}

	return strconv.Itoa(lockValue), nil
}

func (p *P2pStore) UnlockRoom(ctx context.Context, roomName livekit.RoomName, uid string) error {
	k := keyLockRoomPrefix + roomNameKey(roomName)

	actualLockValueString, err := p.db.Get(ctx, k)
	if err != nil {
		return errors.Wrap(err, "get actual lock value")
	}

	if actualLockValueString != uid {
		return ErrRoomLockFailed
	}

	err = p.db.Remove(ctx, k)
	if err != nil {
		return errors.Wrap(err, "delete actual lock")
	}

	return nil
}

func (p *P2pStore) DeleteRoom(ctx context.Context, roomName livekit.RoomName) error {
	err := p.db.Remove(ctx, roomNameKey(roomName))
	if err != nil {
		return errors.Wrap(err, "remove room key")
	}

	return nil
}

func (p *P2pStore) StoreParticipant(ctx context.Context, roomName livekit.RoomName, participant *livekit.ParticipantInfo) error {
	marshaledParticipant, err := proto.Marshal(participant)
	if err != nil {
		return errors.Wrap(err, "marshal participant")
	}

	k := keyPrefixParticipant + string(roomName) + "_" + string(participant.Identity)
	err = p.db.Set(ctx, k, string(marshaledParticipant))
	if err != nil {
		return errors.Wrap(err, "save participant")
	}

	return nil
}

func (p *P2pStore) LoadParticipant(ctx context.Context, roomName livekit.RoomName, identity livekit.ParticipantIdentity) (*livekit.ParticipantInfo, error) {
	res, err := p.db.Get(ctx, participantKey(roomName, identity))
	if err != nil {
		return nil, errors.Wrap(err, "save participant")
	}

	participant := &livekit.ParticipantInfo{}
	err = proto.Unmarshal([]byte(res), participant)
	if err != nil {
		return nil, errors.Wrap(err, "unmarshal participant")
	}

	return participant, nil
}

func (p *P2pStore) ListParticipants(ctx context.Context, roomName livekit.RoomName) ([]*livekit.ParticipantInfo, error) {
	keys, err := p.db.List(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "list keys")
	}

	var res []*livekit.ParticipantInfo
	for _, k := range keys {
		// ignore non participant prefix
		if !strings.HasPrefix(k, keyPrefixParticipant) {
			continue
		}

		// ignore another rooms participant
		if !strings.HasPrefix(strings.TrimLeft(k, keyPrefixParticipant), string(roomName)) {
			continue
		}

		participantDbValue, err := p.db.Get(ctx, k)
		if err != nil {
			return nil, errors.Wrap(err, "get participant")
		}

		participant := &livekit.ParticipantInfo{}
		err = proto.Unmarshal([]byte(participantDbValue), participant)
		if err != nil {
			return nil, errors.Wrap(err, "unmarshal participant")
		}

		res = append(res, participant)
	}

	return res, nil
}

func (p *P2pStore) DeleteParticipant(ctx context.Context, roomName livekit.RoomName, identity livekit.ParticipantIdentity) error {
	err := p.db.Remove(ctx, participantKey(roomName, identity))
	if err != nil {
		return errors.Wrap(err, "delete participant")
	}

	return nil
}

func participantKey(roomName livekit.RoomName, identity livekit.ParticipantIdentity) string {
	return keyPrefixParticipant + roomNameKey(roomName) + "_" + string(identity)
}

func roomNameKey(roomName livekit.RoomName) string {
	return keyRoomPrefix + string(roomName)
}

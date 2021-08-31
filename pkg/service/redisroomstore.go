package service

import (
	"context"
	"time"

	"github.com/go-redis/redis/v8"
	livekit "github.com/livekit/protocol/proto"
	"github.com/livekit/protocol/utils"
	"github.com/pkg/errors"
	"google.golang.org/protobuf/proto"
)

const (
	// RoomsKey is hash of room_name => Room proto
	RoomsKey = "rooms"

	// RoomIdMap is hash of room_id => room name
	RoomIdMap = "room_id_map"

	// RoomParticipantsPrefix is hash of participant_name => ParticipantInfo
	// a key for each room, with expiration
	RoomParticipantsPrefix = "room_participants:"

	// RoomLockPrefix is a simple key containing a provided lock uid
	RoomLockPrefix = "room_lock:"
)

type RedisRoomStore struct {
	rc  *redis.Client
	ctx context.Context
}

func NewRedisRoomStore(rc *redis.Client) *RedisRoomStore {
	return &RedisRoomStore{
		ctx: context.Background(),
		rc:  rc,
	}
}

func (p *RedisRoomStore) StoreRoom(ctx context.Context, room *livekit.Room) error {
	if room.CreationTime == 0 {
		room.CreationTime = time.Now().Unix()
	}

	data, err := proto.Marshal(room)
	if err != nil {
		return err
	}

	pp := p.rc.Pipeline()
	pp.HSet(p.ctx, RoomIdMap, room.Sid, room.Name)
	pp.HSet(p.ctx, RoomsKey, room.Name, data)

	if _, err = pp.Exec(p.ctx); err != nil {
		return errors.Wrap(err, "could not create room")
	}
	return nil
}

func (p *RedisRoomStore) LoadRoom(ctx context.Context, idOrName string) (*livekit.Room, error) {
	// see if matches any ids
	name, err := p.rc.HGet(p.ctx, RoomIdMap, idOrName).Result()
	if err != nil {
		name = idOrName
	}

	data, err := p.rc.HGet(p.ctx, RoomsKey, name).Result()
	if err != nil {
		if err == redis.Nil {
			err = ErrRoomNotFound
		}
		return nil, err
	}

	room := livekit.Room{}
	err = proto.Unmarshal([]byte(data), &room)
	if err != nil {
		return nil, err
	}

	return &room, nil
}

func (p *RedisRoomStore) ListRooms(ctx context.Context) ([]*livekit.Room, error) {
	items, err := p.rc.HVals(p.ctx, RoomsKey).Result()
	if err != nil && err != redis.Nil {
		return nil, errors.Wrap(err, "could not get rooms")
	}

	rooms := make([]*livekit.Room, 0, len(items))

	for _, item := range items {
		room := livekit.Room{}
		err := proto.Unmarshal([]byte(item), &room)
		if err != nil {
			return nil, err
		}
		rooms = append(rooms, &room)
	}
	return rooms, nil
}

func (p *RedisRoomStore) DeleteRoom(ctx context.Context, idOrName string) error {
	room, err := p.LoadRoom(ctx, idOrName)
	var sid, name string

	if err == ErrRoomNotFound {
		// try to clean up as best as we could
		sid = idOrName
		name = idOrName
	} else if err == nil {
		sid = room.Sid
		name = room.Name
	} else {
		return err
	}

	pp := p.rc.Pipeline()
	pp.HDel(p.ctx, RoomIdMap, sid)
	pp.HDel(p.ctx, RoomsKey, name)
	pp.Del(p.ctx, RoomParticipantsPrefix+name)

	_, err = pp.Exec(p.ctx)
	return err
}

func (p *RedisRoomStore) LockRoom(ctx context.Context, name string, duration time.Duration) (string, error) {
	token := utils.NewGuid("LOCK")
	key := RoomLockPrefix + name

	startTime := time.Now()
	for {
		locked, err := p.rc.SetNX(p.ctx, key, token, duration).Result()
		if err != nil {
			return "", err
		}
		if locked {
			return token, nil
		}

		// stop waiting past lock duration
		if time.Now().Sub(startTime) > duration {
			break
		}

		time.Sleep(100 * time.Millisecond)
	}

	return "", ErrRoomLockFailed
}

func (p *RedisRoomStore) UnlockRoom(ctx context.Context, name string, uid string) error {
	key := RoomLockPrefix + name

	val, err := p.rc.Get(p.ctx, key).Result()
	if err == redis.Nil {
		// already unlocked
		return nil
	} else if err != nil {
		return err
	}

	if val != uid {
		return ErrRoomUnlockFailed
	}
	return p.rc.Del(p.ctx, key).Err()
}

func (p *RedisRoomStore) StoreParticipant(ctx context.Context, roomName string, participant *livekit.ParticipantInfo) error {
	key := RoomParticipantsPrefix + roomName

	data, err := proto.Marshal(participant)
	if err != nil {
		return err
	}

	return p.rc.HSet(p.ctx, key, participant.Identity, data).Err()
}

func (p *RedisRoomStore) LoadParticipant(ctx context.Context, roomName, identity string) (*livekit.ParticipantInfo, error) {
	key := RoomParticipantsPrefix + roomName
	data, err := p.rc.HGet(p.ctx, key, identity).Result()
	if err == redis.Nil {
		return nil, ErrParticipantNotFound
	} else if err != nil {
		return nil, err
	}

	pi := livekit.ParticipantInfo{}
	if err := proto.Unmarshal([]byte(data), &pi); err != nil {
		return nil, err
	}
	return &pi, nil
}

func (p *RedisRoomStore) ListParticipants(ctx context.Context, roomName string) ([]*livekit.ParticipantInfo, error) {
	key := RoomParticipantsPrefix + roomName
	items, err := p.rc.HVals(p.ctx, key).Result()
	if err == redis.Nil {
		return nil, nil
	} else if err != nil {
		return nil, err
	}

	participants := make([]*livekit.ParticipantInfo, 0, len(items))
	for _, item := range items {
		pi := livekit.ParticipantInfo{}
		if err := proto.Unmarshal([]byte(item), &pi); err != nil {
			return nil, err
		}
		participants = append(participants, &pi)
	}
	return participants, nil
}

func (p *RedisRoomStore) DeleteParticipant(ctx context.Context, roomName, identity string) error {
	key := RoomParticipantsPrefix + roomName

	return p.rc.HDel(p.ctx, key, identity).Err()
}

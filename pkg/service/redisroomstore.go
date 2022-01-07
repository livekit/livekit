package service

import (
	"context"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/utils"
	"github.com/pkg/errors"
	"google.golang.org/protobuf/proto"
)

const (
	// RoomsKey is hash of room_name => Room proto
	RoomsKey = "rooms"

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

func (p *RedisRoomStore) StoreRoom(_ context.Context, room *livekit.Room) error {
	if room.CreationTime == 0 {
		room.CreationTime = time.Now().Unix()
	}

	data, err := proto.Marshal(room)
	if err != nil {
		return err
	}

	pp := p.rc.Pipeline()
	pp.HSet(p.ctx, RoomsKey, room.Name, data)

	if _, err = pp.Exec(p.ctx); err != nil {
		return errors.Wrap(err, "could not create room")
	}
	return nil
}

func (p *RedisRoomStore) LoadRoom(_ context.Context, name livekit.RoomName) (*livekit.Room, error) {
	data, err := p.rc.HGet(p.ctx, RoomsKey, string(name)).Result()
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

func (p *RedisRoomStore) ListRooms(_ context.Context, names []livekit.RoomName) ([]*livekit.Room, error) {
	var items []string
	var err error
	if names == nil {
		items, err = p.rc.HVals(p.ctx, RoomsKey).Result()
		if err != nil && err != redis.Nil {
			return nil, errors.Wrap(err, "could not get rooms")
		}
	} else {
		roomNames := livekit.RoomNamesAsStrings(names)
		var results []interface{}
		results, err = p.rc.HMGet(p.ctx, RoomsKey, roomNames...).Result()
		if err != nil && err != redis.Nil {
			return nil, errors.Wrap(err, "could not get rooms by names")
		}
		for _, r := range results {
			if item, ok := r.(string); ok {
				items = append(items, item)
			}
		}
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

func (p *RedisRoomStore) DeleteRoom(ctx context.Context, name livekit.RoomName) error {
	_, err := p.LoadRoom(ctx, name)
	if err == ErrRoomNotFound {
		return nil
	}

	pp := p.rc.Pipeline()
	pp.HDel(p.ctx, RoomsKey, string(name))
	pp.Del(p.ctx, RoomParticipantsPrefix+string(name))

	_, err = pp.Exec(p.ctx)
	return err
}

func (p *RedisRoomStore) LockRoom(_ context.Context, name livekit.RoomName, duration time.Duration) (string, error) {
	token := utils.NewGuid("LOCK")
	key := RoomLockPrefix + string(name)

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

func (p *RedisRoomStore) UnlockRoom(_ context.Context, name livekit.RoomName, uid string) error {
	key := RoomLockPrefix + string(name)

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

func (p *RedisRoomStore) StoreParticipant(_ context.Context, roomName livekit.RoomName, participant *livekit.ParticipantInfo) error {
	key := RoomParticipantsPrefix + string(roomName)

	data, err := proto.Marshal(participant)
	if err != nil {
		return err
	}

	return p.rc.HSet(p.ctx, key, participant.Identity, data).Err()
}

func (p *RedisRoomStore) LoadParticipant(_ context.Context, roomName livekit.RoomName, identity livekit.ParticipantIdentity) (*livekit.ParticipantInfo, error) {
	key := RoomParticipantsPrefix + string(roomName)
	data, err := p.rc.HGet(p.ctx, key, string(identity)).Result()
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

func (p *RedisRoomStore) ListParticipants(_ context.Context, roomName livekit.RoomName) ([]*livekit.ParticipantInfo, error) {
	key := RoomParticipantsPrefix + string(roomName)
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

func (p *RedisRoomStore) DeleteParticipant(_ context.Context, roomName livekit.RoomName, identity livekit.ParticipantIdentity) error {
	key := RoomParticipantsPrefix + string(roomName)

	return p.rc.HDel(p.ctx, key, string(identity)).Err()
}

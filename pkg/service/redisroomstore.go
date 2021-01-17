package service

import (
	"context"

	"github.com/go-redis/redis/v8"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/proto/livekit"
)

const (
	// hash of room_name => Room proto
	RoomsKey = "rooms"

	// hash of room_id => room name
	RoomIdMap = "room_id_map"
)

type RedisRoomStore struct {
	rc  *redis.Client
	ctx context.Context
}

func NewRedisRoomStore(rc *redis.Client) *RedisRoomStore {
	return &RedisRoomStore{
		rc: rc,
	}
}

func (p *RedisRoomStore) CreateRoom(room *livekit.Room) error {
	err := p.rc.HSet(p.ctx, RoomIdMap, room.Sid, room.Name).Err()
	if err != nil {
		return err
	}

	data, err := proto.Marshal(room)
	if err != nil {
		return err
	}
	return p.rc.HSet(p.ctx, RoomsKey, room.Name, data).Err()
}

func (p *RedisRoomStore) GetRoom(idOrName string) (*livekit.Room, error) {
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

func (p *RedisRoomStore) ListRooms() ([]*livekit.Room, error) {
	items, err := p.rc.HVals(p.ctx, RoomsKey).Result()
	if err != nil && err != redis.Nil {
		return nil, err
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

func (p *RedisRoomStore) DeleteRoom(idOrName string) error {
	room, err := p.GetRoom(idOrName)
	if err == ErrRoomNotFound {
		return nil
	} else if err != nil {
		return err
	}

	err = p.rc.HDel(p.ctx, RoomIdMap, room.Sid).Err()
	err2 := p.rc.HDel(p.ctx, RoomsKey, room.Name).Err()
	if err == nil {
		err = err2
	}
	return err
}

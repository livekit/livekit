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

	// EgressKey is a hash of egressID => egress info
	EgressKey        = "egress"
	RoomEgressPrefix = "room_egress:"

	// RoomParticipantsPrefix is hash of participant_name => ParticipantInfo
	RoomParticipantsPrefix = "room_participants:"

	// RoomLockPrefix is a simple key containing a provided lock uid
	RoomLockPrefix = "room_lock:"
)

type RedisStore struct {
	rc  *redis.Client
	ctx context.Context
}

func NewRedisStore(rc *redis.Client) *RedisStore {
	return &RedisStore{
		ctx: context.Background(),
		rc:  rc,
	}
}

func (s *RedisStore) StoreRoom(_ context.Context, room *livekit.Room) error {
	if room.CreationTime == 0 {
		room.CreationTime = time.Now().Unix()
	}

	data, err := proto.Marshal(room)
	if err != nil {
		return err
	}

	pp := s.rc.Pipeline()
	pp.HSet(s.ctx, RoomsKey, room.Name, data)

	if _, err = pp.Exec(s.ctx); err != nil {
		return errors.Wrap(err, "could not create room")
	}
	return nil
}

func (s *RedisStore) LoadRoom(_ context.Context, name livekit.RoomName) (*livekit.Room, error) {
	data, err := s.rc.HGet(s.ctx, RoomsKey, string(name)).Result()
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

func (s *RedisStore) ListRooms(_ context.Context, names []livekit.RoomName) ([]*livekit.Room, error) {
	var items []string
	var err error
	if names == nil {
		items, err = s.rc.HVals(s.ctx, RoomsKey).Result()
		if err != nil && err != redis.Nil {
			return nil, errors.Wrap(err, "could not get rooms")
		}
	} else {
		roomNames := livekit.RoomNamesAsStrings(names)
		var results []interface{}
		results, err = s.rc.HMGet(s.ctx, RoomsKey, roomNames...).Result()
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

func (s *RedisStore) DeleteRoom(ctx context.Context, name livekit.RoomName) error {
	_, err := s.LoadRoom(ctx, name)
	if err == ErrRoomNotFound {
		return nil
	}

	pp := s.rc.Pipeline()
	pp.HDel(s.ctx, RoomsKey, string(name))
	pp.Del(s.ctx, RoomParticipantsPrefix+string(name))

	_, err = pp.Exec(s.ctx)
	return err
}

func (s *RedisStore) LockRoom(_ context.Context, name livekit.RoomName, duration time.Duration) (string, error) {
	token := utils.NewGuid("LOCK")
	key := RoomLockPrefix + string(name)

	startTime := time.Now()
	for {
		locked, err := s.rc.SetNX(s.ctx, key, token, duration).Result()
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

func (s *RedisStore) UnlockRoom(_ context.Context, name livekit.RoomName, uid string) error {
	key := RoomLockPrefix + string(name)

	val, err := s.rc.Get(s.ctx, key).Result()
	if err == redis.Nil {
		// already unlocked
		return nil
	} else if err != nil {
		return err
	}

	if val != uid {
		return ErrRoomUnlockFailed
	}
	return s.rc.Del(s.ctx, key).Err()
}

func (s *RedisStore) StoreParticipant(_ context.Context, roomName livekit.RoomName, participant *livekit.ParticipantInfo) error {
	key := RoomParticipantsPrefix + string(roomName)

	data, err := proto.Marshal(participant)
	if err != nil {
		return err
	}

	return s.rc.HSet(s.ctx, key, participant.Identity, data).Err()
}

func (s *RedisStore) LoadParticipant(_ context.Context, roomName livekit.RoomName, identity livekit.ParticipantIdentity) (*livekit.ParticipantInfo, error) {
	key := RoomParticipantsPrefix + string(roomName)
	data, err := s.rc.HGet(s.ctx, key, string(identity)).Result()
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

func (s *RedisStore) ListParticipants(_ context.Context, roomName livekit.RoomName) ([]*livekit.ParticipantInfo, error) {
	key := RoomParticipantsPrefix + string(roomName)
	items, err := s.rc.HVals(s.ctx, key).Result()
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

func (s *RedisStore) DeleteParticipant(_ context.Context, roomName livekit.RoomName, identity livekit.ParticipantIdentity) error {
	key := RoomParticipantsPrefix + string(roomName)

	return s.rc.HDel(s.ctx, key, string(identity)).Err()
}

func (s *RedisStore) StoreEgress(_ context.Context, info *livekit.EgressInfo) error {
	data, err := proto.Marshal(info)
	if err != nil {
		return err
	}

	pp := s.rc.Pipeline()
	pp.HSet(s.ctx, EgressKey, info.EgressId, data)
	pp.SAdd(s.ctx, RoomEgressPrefix+info.RoomId, info.EgressId)

	if _, err = pp.Exec(s.ctx); err != nil {
		return errors.Wrap(err, "could not store egress info")
	}

	return nil
}

func (s *RedisStore) LoadEgress(_ context.Context, egressID string) (*livekit.EgressInfo, error) {
	data, err := s.rc.HGet(s.ctx, EgressKey, egressID).Result()
	if err != nil {
		if err == redis.Nil {
			return nil, ErrEgressNotFound
		}
		return nil, err
	}

	info := &livekit.EgressInfo{}
	err = proto.Unmarshal([]byte(data), info)
	if err != nil {
		return nil, err
	}
	return info, nil
}

func (s *RedisStore) ListEgress(_ context.Context, roomID livekit.RoomID) ([]*livekit.EgressInfo, error) {
	var infos []*livekit.EgressInfo

	if roomID == "" {
		data, err := s.rc.HGetAll(s.ctx, EgressKey).Result()
		if err != nil {
			if err == redis.Nil {
				return nil, nil
			}
			return nil, err
		}

		for _, d := range data {
			info := &livekit.EgressInfo{}
			err = proto.Unmarshal([]byte(d), info)
			if err != nil {
				return nil, err
			}
			infos = append(infos, info)
		}
	} else {
		ids, err := s.rc.SMembers(s.ctx, RoomEgressPrefix+string(roomID)).Result()
		if err != nil {
			if err == redis.Nil {
				return nil, nil
			}
			return nil, err
		}

		data, err := s.rc.HMGet(s.ctx, EgressKey, ids...).Result()
		for _, d := range data {
			if d == nil {
				continue
			}
			info := &livekit.EgressInfo{}
			err = proto.Unmarshal([]byte(d.(string)), info)
			if err != nil {
				return nil, err
			}
			infos = append(infos, info)
		}
	}

	return infos, nil
}

func (s *RedisStore) UpdateEgress(_ context.Context, info *livekit.EgressInfo) error {
	data, err := proto.Marshal(info)
	if err != nil {
		return err
	}

	pp := s.rc.Pipeline()
	pp.HSet(s.ctx, EgressKey, info.EgressId, data)

	if _, err = pp.Exec(s.ctx); err != nil {
		return errors.Wrap(err, "could not store egress info")
	}

	return nil
}

func (s *RedisStore) DeleteEgress(_ context.Context, info *livekit.EgressInfo) error {
	err := s.rc.SRem(s.ctx, RoomEgressPrefix+info.RoomId, info.EgressId).Err()
	if err != nil {
		return err
	}

	return s.rc.HDel(s.ctx, EgressKey, info.EgressId).Err()
}

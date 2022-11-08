package service

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/go-redis/redis/v8"
	goversion "github.com/hashicorp/go-version"
	"github.com/pkg/errors"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/version"
	"github.com/livekit/protocol/ingress"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"
)

const (
	VersionKey = "livekit_version"

	// RoomsKey is hash of room_name => Room proto
	RoomsKey        = "rooms"
	RoomInternalKey = "room_internal"

	// EgressKey is a hash of egressID => egress info
	EgressKey                  = "egress"
	EndedEgressKey             = "ended_egress"
	RoomEgressPrefix           = "egress:room:"
	DeprecatedRoomEgressPrefix = "room_egress:"

	// IngressKey is a hash of ingressID => ingress info
	IngressKey         = "ingress"
	StreamKeyKey       = "stream_key"
	IngressStatePrefix = "ingress_state:"
	RoomIngressPrefix  = "room_ingress:"

	// RoomParticipantsPrefix is hash of participant_name => ParticipantInfo
	RoomParticipantsPrefix = "room_participants:"

	// RoomLockPrefix is a simple key containing a provided lock uid
	RoomLockPrefix = "room_lock:"

	maxRetries = 5
)

type RedisStore struct {
	rc           redis.UniversalClient
	unlockScript *redis.Script
	ctx          context.Context
	done         chan struct{}
}

func NewRedisStore(rc redis.UniversalClient) *RedisStore {
	unlockScript := `if redis.call("get", KEYS[1]) == ARGV[1] then
						return redis.call("del", KEYS[1])
					 else return 0 
					 end`

	return &RedisStore{
		ctx:          context.Background(),
		rc:           rc,
		unlockScript: redis.NewScript(unlockScript),
	}
}

func (s *RedisStore) Start() error {
	if s.done != nil {
		return nil
	}

	s.done = make(chan struct{}, 1)
	current, err := s.rc.Get(s.ctx, VersionKey).Result()
	if err != nil && err != redis.Nil {
		return err
	}
	if current == "" {
		current = "0.0.0"
	}

	v, _ := goversion.NewVersion(current)
	migrateEgress, _ := goversion.NewVersion("1.1.3")
	if v.LessThan(migrateEgress) {
		if _, err = s.MigrateEgressInfo(); err != nil {
			return err
		}

		if err = s.rc.Set(s.ctx, VersionKey, version.Version, 0).Err(); err != nil {
			return err
		}
	}

	go s.egressWorker()
	return nil
}

func (s *RedisStore) Stop() {
	select {
	case <-s.done:
	default:
		close(s.done)
	}
}

func (s *RedisStore) StoreRoom(_ context.Context, room *livekit.Room, internal *livekit.RoomInternal) error {
	if room.CreationTime == 0 {
		room.CreationTime = time.Now().Unix()
	}

	roomData, err := proto.Marshal(room)
	if err != nil {
		return err
	}

	pp := s.rc.Pipeline()
	pp.HSet(s.ctx, RoomsKey, room.Name, roomData)

	var internalData []byte
	if internal != nil {
		internalData, err = proto.Marshal(internal)
		if err != nil {
			return err
		}
		pp.HSet(s.ctx, RoomInternalKey, room.Name, internalData)
	} else {
		pp.HDel(s.ctx, RoomInternalKey, room.Name)
	}

	if _, err = pp.Exec(s.ctx); err != nil {
		return errors.Wrap(err, "could not create room")
	}
	return nil
}

func (s *RedisStore) LoadRoom(_ context.Context, roomName livekit.RoomName, includeInternal bool) (*livekit.Room, *livekit.RoomInternal, error) {
	pp := s.rc.Pipeline()
	pp.HGet(s.ctx, RoomsKey, string(roomName))
	if includeInternal {
		pp.HGet(s.ctx, RoomInternalKey, string(roomName))
	}

	res, err := pp.Exec(s.ctx)
	if err != nil && err != redis.Nil {
		// if the room exists but internal does not, the pipeline will still return redis.Nil
		return nil, nil, err
	}

	room := &livekit.Room{}
	roomData, err := res[0].(*redis.StringCmd).Result()
	if err != nil {
		if err == redis.Nil {
			err = ErrRoomNotFound
		}
		return nil, nil, err
	}
	if err = proto.Unmarshal([]byte(roomData), room); err != nil {
		return nil, nil, err
	}

	var internal *livekit.RoomInternal
	if includeInternal {
		internalData, err := res[1].(*redis.StringCmd).Result()
		if err == nil {
			internal = &livekit.RoomInternal{}
			if err = proto.Unmarshal([]byte(internalData), internal); err != nil {
				return nil, nil, err
			}
		} else if err != redis.Nil {
			return nil, nil, err
		}
	}

	return room, internal, nil
}

func (s *RedisStore) ListRooms(_ context.Context, roomNames []livekit.RoomName) ([]*livekit.Room, error) {
	var items []string
	var err error
	if roomNames == nil {
		items, err = s.rc.HVals(s.ctx, RoomsKey).Result()
		if err != nil && err != redis.Nil {
			return nil, errors.Wrap(err, "could not get rooms")
		}
	} else {
		names := livekit.RoomNamesAsStrings(roomNames)
		var results []interface{}
		results, err = s.rc.HMGet(s.ctx, RoomsKey, names...).Result()
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

func (s *RedisStore) DeleteRoom(ctx context.Context, roomName livekit.RoomName) error {
	_, _, err := s.LoadRoom(ctx, roomName, false)
	if err == ErrRoomNotFound {
		return nil
	}

	pp := s.rc.Pipeline()
	pp.HDel(s.ctx, RoomsKey, string(roomName))
	pp.HDel(s.ctx, RoomInternalKey, string(roomName))
	pp.Del(s.ctx, RoomParticipantsPrefix+string(roomName))

	_, err = pp.Exec(s.ctx)
	return err
}

func (s *RedisStore) LockRoom(_ context.Context, roomName livekit.RoomName, duration time.Duration) (string, error) {
	token := utils.NewGuid("LOCK")
	key := RoomLockPrefix + string(roomName)

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
		if time.Since(startTime) > duration {
			break
		}

		time.Sleep(100 * time.Millisecond)
	}

	return "", ErrRoomLockFailed
}

func (s *RedisStore) UnlockRoom(ctx context.Context, roomName livekit.RoomName, uid string) error {
	key := RoomLockPrefix + string(roomName)
	res, err := s.unlockScript.Run(ctx, s.rc, []string{key}, uid).Result()
	if err != nil {
		return err
	}

	// uid does not match
	if i, ok := res.(int64); !ok || i != 1 {
		return ErrRoomUnlockFailed
	}

	return nil
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

	tx := s.rc.TxPipeline()
	tx.HSet(s.ctx, EgressKey, info.EgressId, data)
	tx.SAdd(s.ctx, RoomEgressPrefix+info.RoomName, info.EgressId)
	if _, err = tx.Exec(s.ctx); err != nil {
		return errors.Wrap(err, "could not store egress info")
	}

	return nil
}

func (s *RedisStore) LoadEgress(_ context.Context, egressID string) (*livekit.EgressInfo, error) {
	data, err := s.rc.HGet(s.ctx, EgressKey, egressID).Result()
	switch err {
	case nil:
		info := &livekit.EgressInfo{}
		err = proto.Unmarshal([]byte(data), info)
		if err != nil {
			return nil, err
		}
		return info, nil

	case redis.Nil:
		return nil, ErrEgressNotFound

	default:
		return nil, err
	}
}

func (s *RedisStore) ListEgress(_ context.Context, roomName livekit.RoomName) ([]*livekit.EgressInfo, error) {
	var infos []*livekit.EgressInfo

	if roomName == "" {
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
		egressIDs, err := s.rc.SMembers(s.ctx, RoomEgressPrefix+string(roomName)).Result()
		if err != nil {
			if err == redis.Nil {
				return nil, nil
			}
			return nil, err
		}

		data, _ := s.rc.HMGet(s.ctx, EgressKey, egressIDs...).Result()
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

	if info.EndedAt != 0 {
		tx := s.rc.TxPipeline()
		tx.HSet(s.ctx, EgressKey, info.EgressId, data)
		tx.HSet(s.ctx, EndedEgressKey, info.EgressId, egressEndedValue(info.RoomName, info.EndedAt))
		_, err = tx.Exec(s.ctx)
	} else {
		err = s.rc.HSet(s.ctx, EgressKey, info.EgressId, data).Err()
	}

	if err != nil {
		return errors.Wrap(err, "could not update egress info")
	}

	return nil
}

// Deletes egress info 24h after the egress has ended
func (s *RedisStore) egressWorker() {
	ticker := time.NewTicker(time.Minute * 30)
	defer ticker.Stop()

	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			err := s.CleanEndedEgress()
			if err != nil {
				logger.Errorw("could not clean egress info", err)
			}
		}
	}
}

func (s RedisStore) CleanEndedEgress() error {
	values, err := s.rc.HGetAll(s.ctx, EndedEgressKey).Result()
	if err != nil && err != redis.Nil {
		return err
	}

	expiry := time.Now().Add(-24 * time.Hour).UnixNano()
	for egressID, val := range values {
		roomName, endedAt, err := parseEgressEnded(val)
		if err != nil {
			return err
		}

		if endedAt < expiry {
			tx := s.rc.TxPipeline()
			tx.HDel(s.ctx, EndedEgressKey, egressID)
			tx.SRem(s.ctx, RoomEgressPrefix+roomName, egressID)
			tx.HDel(s.ctx, EgressKey, egressID)
			if _, err := tx.Exec(s.ctx); err != nil {
				return err
			}
		}
	}

	return nil
}

func egressEndedValue(roomName string, endedAt int64) string {
	return fmt.Sprintf("%s|%d", roomName, endedAt)
}

func parseEgressEnded(value string) (roomName string, endedAt int64, err error) {
	s := strings.Split(value, "|")
	if len(s) != 2 {
		err = errors.New("invalid egressEnded value")
		return
	}

	roomName = s[0]
	endedAt, err = strconv.ParseInt(s[1], 10, 64)
	return
}

func (s *RedisStore) StoreIngress(ctx context.Context, info *livekit.IngressInfo) error {
	err := s.storeIngress(ctx, info)
	if err != nil {
		return err
	}

	return s.storeIngressState(ctx, info.IngressId, nil)
}

func (s *RedisStore) storeIngress(_ context.Context, info *livekit.IngressInfo) error {
	if info.IngressId == "" {
		return errors.New("Missing IngressId")
	}
	if info.StreamKey == "" {
		return errors.New("Missing StreamKey")
	}

	// ignore state
	infoCopy := livekit.IngressInfo{}
	infoCopy = *info
	infoCopy.State = nil

	data, err := proto.Marshal(&infoCopy)
	if err != nil {
		return err
	}

	// Use a "transaction" to remove the old room association if it changed
	txf := func(tx *redis.Tx) error {
		var oldRoom string

		oldInfo, err := s.loadIngress(tx, info.IngressId)
		switch err {
		case ErrIngressNotFound:
			// Ingress doesn't exist yet
		case nil:
			oldRoom = oldInfo.RoomName
		default:
			return err
		}

		results, err := tx.TxPipelined(s.ctx, func(p redis.Pipeliner) error {
			p.HSet(s.ctx, IngressKey, info.IngressId, data)
			p.HSet(s.ctx, StreamKeyKey, info.StreamKey, info.IngressId)

			if oldRoom != info.RoomName {
				if oldRoom != "" {
					p.SRem(s.ctx, RoomIngressPrefix+oldRoom, info.IngressId)
				}
				if info.RoomName != "" {
					p.SAdd(s.ctx, RoomIngressPrefix+info.RoomName, info.IngressId)
				}
			}

			return nil
		})

		if err != nil {
			return err
		}

		for _, res := range results {
			if err := res.Err(); err != nil {
				return err
			}
		}

		return nil
	}

	// Retry if the key has been changed.
	for i := 0; i < maxRetries; i++ {
		err := s.rc.Watch(s.ctx, txf, IngressKey)
		switch err {
		case redis.TxFailedErr:
			// Optimistic lock lost. Retry.
			continue
		default:
			return err
		}
	}

	return nil
}

func (s *RedisStore) storeIngressState(_ context.Context, ingressId string, state *livekit.IngressState) error {
	if ingressId == "" {
		return errors.New("Missing IngressId")
	}

	if state == nil {
		state = &livekit.IngressState{}
	}

	data, err := proto.Marshal(state)
	if err != nil {
		return err
	}

	// Use a "transaction" to remove the old room association if it changed
	txf := func(tx *redis.Tx) error {
		var oldStartedAt int64

		info, err := s.loadIngress(tx, ingressId)
		if err != nil {
			return err
		}

		oldState, err := s.loadIngressState(tx, ingressId)
		switch err {
		case ErrIngressNotFound:
			// Ingress state doesn't exist yet
		case nil:
			oldStartedAt = oldState.StartedAt
		default:
			return err
		}

		results, err := tx.TxPipelined(s.ctx, func(p redis.Pipeliner) error {
			if state.StartedAt < oldStartedAt {
				// Do not overwrite the info and state of a more recent session
				return ingress.ErrIngressOutOfDate
			}

			p.Set(s.ctx, IngressStatePrefix+ingressId, data, 0)
			p.HSet(s.ctx, StreamKeyKey, info.StreamKey, info.IngressId)

			return nil
		})

		if err != nil {
			return err
		}

		for _, res := range results {
			if err := res.Err(); err != nil {
				return err
			}
		}

		return nil
	}

	// Retry if the key has been changed.
	for i := 0; i < maxRetries; i++ {
		err := s.rc.Watch(s.ctx, txf, IngressKey, IngressStatePrefix+ingressId)
		switch err {
		case redis.TxFailedErr:
			// Optimistic lock lost. Retry.
			continue
		default:
			return err
		}
	}

	return nil
}

func (s *RedisStore) loadIngress(c redis.Cmdable, ingressId string) (*livekit.IngressInfo, error) {
	data, err := c.HGet(s.ctx, IngressKey, ingressId).Result()
	switch err {
	case nil:
		info := &livekit.IngressInfo{}
		err = proto.Unmarshal([]byte(data), info)
		if err != nil {
			return nil, err
		}
		return info, nil

	case redis.Nil:
		return nil, ErrIngressNotFound

	default:
		return nil, err
	}
}

func (s *RedisStore) loadIngressState(c redis.Cmdable, ingressId string) (*livekit.IngressState, error) {
	data, err := c.Get(s.ctx, IngressStatePrefix+ingressId).Result()
	switch err {
	case nil:
		state := &livekit.IngressState{}
		err = proto.Unmarshal([]byte(data), state)
		if err != nil {
			return nil, err
		}
		return state, nil

	case redis.Nil:
		return nil, ErrIngressNotFound

	default:
		return nil, err
	}
}

func (s *RedisStore) LoadIngress(_ context.Context, ingressId string) (*livekit.IngressInfo, error) {
	info, err := s.loadIngress(s.rc, ingressId)
	if err != nil {
		return nil, err
	}
	state, err := s.loadIngressState(s.rc, ingressId)
	switch err {
	case nil:
		info.State = state
	case ErrIngressNotFound:
		// No state for this ingress
	default:
		return nil, err
	}

	return info, nil
}

func (s *RedisStore) LoadIngressFromStreamKey(_ context.Context, streamKey string) (*livekit.IngressInfo, error) {
	ingressID, err := s.rc.HGet(s.ctx, StreamKeyKey, streamKey).Result()
	switch err {
	case nil:
		return s.LoadIngress(s.ctx, ingressID)

	case redis.Nil:
		return nil, ErrIngressNotFound

	default:
		return nil, err
	}
}

func (s *RedisStore) ListIngress(ctx context.Context, roomName livekit.RoomName) ([]*livekit.IngressInfo, error) {
	var infos []*livekit.IngressInfo

	if roomName == "" {
		data, err := s.rc.HGetAll(s.ctx, IngressKey).Result()
		if err != nil {
			if err == redis.Nil {
				return nil, nil
			}
			return nil, err
		}

		for _, d := range data {
			info := &livekit.IngressInfo{}
			err = proto.Unmarshal([]byte(d), info)
			if err != nil {
				return nil, err
			}
			state, err := s.loadIngressState(s.rc, info.IngressId)
			switch err {
			case nil:
				info.State = state
			case ErrIngressNotFound:
				// No state for this ingress
			default:
				return nil, err
			}

			infos = append(infos, info)
		}
	} else {
		ingressIDs, err := s.rc.SMembers(s.ctx, RoomIngressPrefix+string(roomName)).Result()
		if err != nil {
			if err == redis.Nil {
				return nil, nil
			}
			return nil, err
		}

		data, _ := s.rc.HMGet(s.ctx, IngressKey, ingressIDs...).Result()
		for _, d := range data {
			if d == nil {
				continue
			}
			info := &livekit.IngressInfo{}
			err = proto.Unmarshal([]byte(d.(string)), info)
			if err != nil {
				return nil, err
			}
			state, err := s.loadIngressState(s.rc, info.IngressId)
			switch err {
			case nil:
				info.State = state
			case ErrIngressNotFound:
				// No state for this ingress
			default:
				return nil, err
			}

			infos = append(infos, info)
		}
	}

	return infos, nil
}

func (s *RedisStore) UpdateIngress(ctx context.Context, info *livekit.IngressInfo) error {
	return s.storeIngress(ctx, info)
}

func (s *RedisStore) UpdateIngressState(ctx context.Context, ingressId string, state *livekit.IngressState) error {
	return s.storeIngressState(ctx, ingressId, state)
}

func (s *RedisStore) DeleteIngress(_ context.Context, info *livekit.IngressInfo) error {
	tx := s.rc.TxPipeline()
	tx.SRem(s.ctx, RoomIngressPrefix+info.RoomName, info.IngressId)
	tx.HDel(s.ctx, StreamKeyKey, info.IngressId)
	tx.HDel(s.ctx, IngressKey, info.IngressId)
	tx.Del(s.ctx, IngressStatePrefix+info.IngressId)
	if _, err := tx.Exec(s.ctx); err != nil {
		return errors.Wrap(err, "could not delete ingress info")
	}

	return nil
}

// Migration to LiveKit >= v1.1.3
func (s *RedisStore) MigrateEgressInfo() (int, error) {
	locked, err := s.rc.SetNX(s.ctx, "egress-migration", utils.NewGuid("LOCK"), time.Minute).Result()
	if err != nil {
		return 0, err
	} else if !locked {
		return 0, nil
	}

	it := s.rc.Scan(s.ctx, 0, DeprecatedRoomEgressPrefix+"*", 0).Iterator()
	migrated := 0
	for it.Next(s.ctx) {
		migrated++
		key := it.Val()
		egressIDs, err := s.rc.SMembers(s.ctx, key).Result()
		if err != nil && err != redis.Nil {
			return migrated, err
		}

		for _, egressID := range egressIDs {
			info, err := s.LoadEgress(s.ctx, egressID)
			if err != nil {
				return migrated, err
			}

			var roomName string
			switch req := info.Request.(type) {
			case *livekit.EgressInfo_RoomComposite:
				roomName = req.RoomComposite.RoomName
			case *livekit.EgressInfo_TrackComposite:
				roomName = req.TrackComposite.RoomName
			case *livekit.EgressInfo_Track:
				roomName = req.Track.RoomName
			}

			tx := s.rc.TxPipeline()
			tx.SAdd(s.ctx, RoomEgressPrefix+roomName, egressID)
			tx.SRem(s.ctx, key, egressID)
			if _, err = tx.Exec(s.ctx); err != nil {
				return migrated, err
			}
		}

		s.rc.Del(s.ctx, key)
	}

	return migrated, nil
}

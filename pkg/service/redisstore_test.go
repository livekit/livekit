package service_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/atomic"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/protocol/ingress"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/utils"

	"github.com/livekit/livekit-server/pkg/service"
)

func TestRoomInternal(t *testing.T) {
	ctx := context.Background()
	rs := service.NewRedisStore(redisClient())

	room := &livekit.Room{
		Sid:  "123",
		Name: "test_room",
	}
	internal := &livekit.RoomInternal{
		TrackEgress: &livekit.AutoTrackEgress{Filepath: "egress"},
	}

	require.NoError(t, rs.StoreRoom(ctx, room, internal))
	actualRoom, actualInternal, err := rs.LoadRoom(ctx, livekit.RoomName(room.Name), true)
	require.NoError(t, err)
	require.Equal(t, room.Sid, actualRoom.Sid)
	require.Equal(t, internal.TrackEgress.Filepath, actualInternal.TrackEgress.Filepath)

	// remove internal
	require.NoError(t, rs.StoreRoom(ctx, room, nil))
	_, actualInternal, err = rs.LoadRoom(ctx, livekit.RoomName(room.Name), true)
	require.NoError(t, err)
	require.Nil(t, actualInternal)

	// clean up
	require.NoError(t, rs.DeleteRoom(ctx, "test_room"))
}

func TestParticipantPersistence(t *testing.T) {
	ctx := context.Background()
	rs := service.NewRedisStore(redisClient())

	roomName := livekit.RoomName("room1")
	_ = rs.DeleteRoom(ctx, roomName)

	p := &livekit.ParticipantInfo{
		Sid:      "PA_test",
		Identity: "test",
		State:    livekit.ParticipantInfo_ACTIVE,
		Tracks: []*livekit.TrackInfo{
			{
				Sid:  "track1",
				Type: livekit.TrackType_AUDIO,
				Name: "audio",
			},
		},
	}

	// create the participant
	require.NoError(t, rs.StoreParticipant(ctx, roomName, p))

	// result should match
	pGet, err := rs.LoadParticipant(ctx, roomName, livekit.ParticipantIdentity(p.Identity))
	require.NoError(t, err)
	require.Equal(t, p.Identity, pGet.Identity)
	require.Equal(t, len(p.Tracks), len(pGet.Tracks))
	require.Equal(t, p.Tracks[0].Sid, pGet.Tracks[0].Sid)

	// list should return one participant
	participants, err := rs.ListParticipants(ctx, roomName)
	require.NoError(t, err)
	require.Len(t, participants, 1)

	// deleting participant should return to normal
	require.NoError(t, rs.DeleteParticipant(ctx, roomName, livekit.ParticipantIdentity(p.Identity)))

	participants, err = rs.ListParticipants(ctx, roomName)
	require.NoError(t, err)
	require.Len(t, participants, 0)

	// shouldn't be able to get it
	_, err = rs.LoadParticipant(ctx, roomName, livekit.ParticipantIdentity(p.Identity))
	require.Equal(t, err, service.ErrParticipantNotFound)
}

func TestRoomLock(t *testing.T) {
	ctx := context.Background()
	rs := service.NewRedisStore(redisClient())
	lockInterval := 5 * time.Millisecond
	roomName := livekit.RoomName("myroom")

	t.Run("normal locking", func(t *testing.T) {
		token, err := rs.LockRoom(ctx, roomName, lockInterval)
		require.NoError(t, err)
		require.NotEmpty(t, token)
		require.NoError(t, rs.UnlockRoom(ctx, roomName, token))
	})

	t.Run("waits before acquiring lock", func(t *testing.T) {
		token, err := rs.LockRoom(ctx, roomName, lockInterval)
		require.NoError(t, err)
		require.NotEmpty(t, token)
		unlocked := atomic.NewUint32(0)
		wg := sync.WaitGroup{}

		wg.Add(1)
		go func() {
			// attempt to lock again
			defer wg.Done()
			token2, err := rs.LockRoom(ctx, roomName, lockInterval)
			require.NoError(t, err)
			defer rs.UnlockRoom(ctx, roomName, token2)
			require.Equal(t, uint32(1), unlocked.Load())
		}()

		// release after 2 ms
		time.Sleep(2 * time.Millisecond)
		unlocked.Store(1)
		_ = rs.UnlockRoom(ctx, roomName, token)

		wg.Wait()
	})

	t.Run("lock expires", func(t *testing.T) {
		token, err := rs.LockRoom(ctx, roomName, lockInterval)
		require.NoError(t, err)
		defer rs.UnlockRoom(ctx, roomName, token)

		time.Sleep(lockInterval + time.Millisecond)
		token2, err := rs.LockRoom(ctx, roomName, lockInterval)
		require.NoError(t, err)
		_ = rs.UnlockRoom(ctx, roomName, token2)
	})
}

func TestEgressStore(t *testing.T) {
	ctx := context.Background()
	rc := redisClient()
	rs := service.NewRedisStore(rc)

	roomName := "egress-test"

	// test migration
	info := &livekit.EgressInfo{
		EgressId: utils.NewGuid(utils.EgressPrefix),
		RoomId:   utils.NewGuid(utils.RoomPrefix),
		RoomName: roomName,
		Status:   livekit.EgressStatus_EGRESS_STARTING,
		Request: &livekit.EgressInfo_RoomComposite{
			RoomComposite: &livekit.RoomCompositeEgressRequest{
				RoomName: roomName,
				Layout:   "speaker-dark",
			},
		},
	}

	data, err := proto.Marshal(info)
	require.NoError(t, err)

	// store egress info the old way
	tx := rc.TxPipeline()
	tx.HSet(ctx, service.EgressKey, info.EgressId, data)
	tx.SAdd(ctx, service.DeprecatedRoomEgressPrefix+info.RoomId, info.EgressId)
	_, err = tx.Exec(ctx)
	require.NoError(t, err)

	// run migration
	migrated, err := rs.MigrateEgressInfo()
	require.NoError(t, err)
	require.Equal(t, 1, migrated)

	// check that it was migrated
	exists, err := rc.Exists(ctx, service.DeprecatedRoomEgressPrefix+info.RoomId).Result()
	require.NoError(t, err)
	require.Equal(t, int64(0), exists)

	exists, err = rc.Exists(ctx, service.RoomEgressPrefix+info.RoomName).Result()
	require.NoError(t, err)
	require.Equal(t, int64(1), exists)

	// load
	res, err := rs.LoadEgress(ctx, info.EgressId)
	require.NoError(t, err)
	require.Equal(t, res.EgressId, info.EgressId)

	// store another
	info2 := &livekit.EgressInfo{
		EgressId: utils.NewGuid(utils.EgressPrefix),
		RoomId:   utils.NewGuid(utils.RoomPrefix),
		RoomName: "another-egress-test",
		Status:   livekit.EgressStatus_EGRESS_STARTING,
		Request: &livekit.EgressInfo_RoomComposite{
			RoomComposite: &livekit.RoomCompositeEgressRequest{
				RoomName: "another-egress-test",
				Layout:   "speaker-dark",
			},
		},
	}
	require.NoError(t, rs.StoreEgress(ctx, info2))

	// update
	info2.Status = livekit.EgressStatus_EGRESS_COMPLETE
	info2.EndedAt = time.Now().Add(-24 * time.Hour).UnixNano()
	require.NoError(t, rs.UpdateEgress(ctx, info))

	// list
	list, err := rs.ListEgress(ctx, "")
	require.NoError(t, err)
	require.Len(t, list, 2)

	// list by room
	list, err = rs.ListEgress(ctx, livekit.RoomName(roomName))
	require.NoError(t, err)
	require.Len(t, list, 1)

	// update
	info.Status = livekit.EgressStatus_EGRESS_COMPLETE
	info.EndedAt = time.Now().Add(-24 * time.Hour).UnixNano()
	require.NoError(t, rs.UpdateEgress(ctx, info))

	// clean
	require.NoError(t, rs.CleanEndedEgress())

	// list
	list, err = rs.ListEgress(ctx, livekit.RoomName(roomName))
	require.NoError(t, err)
	require.Len(t, list, 0)
}

func TestIngressStore(t *testing.T) {
	ctx := context.Background()
	rs := service.NewRedisStore(redisClient())

	info := &livekit.IngressInfo{
		IngressId: "ingressId",
		StreamKey: "streamKey",
		State: &livekit.IngressState{
			StartedAt: 2,
		},
	}

	err := rs.StoreIngress(ctx, info)
	require.NoError(t, err)

	err = rs.UpdateIngressState(ctx, info.IngressId, info.State)
	require.NoError(t, err)

	t.Cleanup(func() {
		rs.DeleteIngress(ctx, info)
	})

	pulledInfo, err := rs.LoadIngress(ctx, "ingressId")
	require.NoError(t, err)
	compareIngressInfo(t, pulledInfo, info)

	infos, err := rs.ListIngress(ctx, "room")
	require.NoError(t, err)
	require.Equal(t, 0, len(infos))

	info.RoomName = "room"
	err = rs.UpdateIngress(ctx, info)
	require.NoError(t, err)

	infos, err = rs.ListIngress(ctx, "room")
	require.NoError(t, err)

	require.NoError(t, err)
	require.Equal(t, 1, len(infos))
	compareIngressInfo(t, infos[0], info)

	info.RoomName = ""
	err = rs.UpdateIngress(ctx, info)
	require.NoError(t, err)

	infos, err = rs.ListIngress(ctx, "room")
	require.NoError(t, err)
	require.Equal(t, 0, len(infos))

	info.State.StartedAt = 1
	err = rs.UpdateIngressState(ctx, info.IngressId, info.State)
	require.Equal(t, ingress.ErrIngressOutOfDate, err)

	info.State.StartedAt = 3
	err = rs.UpdateIngressState(ctx, info.IngressId, info.State)
	require.NoError(t, err)

	infos, err = rs.ListIngress(ctx, "")
	require.NoError(t, err)
	require.Equal(t, 1, len(infos))
	require.Equal(t, "", infos[0].RoomName)
}

func compareIngressInfo(t *testing.T, expected, v *livekit.IngressInfo) {
	require.Equal(t, expected.IngressId, v.IngressId)
	require.Equal(t, expected.StreamKey, v.StreamKey)
	require.Equal(t, expected.RoomName, v.RoomName)
}

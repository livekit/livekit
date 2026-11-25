// Copyright 2026 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package service_test

import (
	"context"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/service"
)

// watchRival is a go-redis hook that writes to every key the hooked client
// WATCHes, from another connection, right after the WATCH is sent. Every
// optimistic WATCH/MULTI/EXEC transaction the hooked client attempts therefore
// fails with redis.TxFailedErr, the same way a busy cluster can defeat all of
// RedisStore's retries.
type watchRival struct {
	write func(ctx context.Context, key string) error
}

func (r *watchRival) DialHook(next redis.DialHook) redis.DialHook { return next }

func (r *watchRival) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (r *watchRival) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if err := next(ctx, cmd); err != nil {
			return err
		}
		if strings.EqualFold(cmd.Name(), "watch") {
			for _, key := range cmd.Args()[1:] {
				if err := r.write(ctx, key.(string)); err != nil {
					return err
				}
			}
		}
		return nil
	}
}

// RedisStore.storeIngress and storeIngressState retry their optimistic
// transaction a fixed number of times on redis.TxFailedErr. When every attempt
// loses the lock they must report it: returning nil makes the caller believe
// the write landed, so CreateIngress answers with an ingress that does not
// exist and leaves an orphan state key, and UpdateIngressState silently drops
// a state transition.
func TestIngressStoreReportsLostOptimisticLock(t *testing.T) {
	ctx := context.Background()
	rc := redisClient(t)
	rs := service.NewRedisStore(rc)

	// Every transaction attempted through this store loses its optimistic lock.
	rival := &watchRival{}
	contendedClient := redis.NewClient(&redis.Options{Addr: rc.Options().Addr})
	contendedClient.AddHook(rival)
	t.Cleanup(func() { _ = contendedClient.Close() })
	contended := service.NewRedisStore(contendedClient)

	info := &livekit.IngressInfo{
		IngressId: "IN_lost_lock",
		StreamKey: "sk_lost_lock",
		RoomName:  "room_lost_lock",
	}
	t.Cleanup(func() { _ = rs.DeleteIngress(ctx, info) })

	t.Run("StoreIngress", func(t *testing.T) {
		// Any other ingress written to the hash invalidates the WATCH on IngressKey.
		other, err := proto.Marshal(&livekit.IngressInfo{IngressId: "IN_rival"})
		require.NoError(t, err)
		t.Cleanup(func() { _ = rc.HDel(ctx, service.IngressKey, "IN_rival").Err() })
		rival.write = func(ctx context.Context, key string) error {
			if key != service.IngressKey {
				return nil
			}
			return rc.HSet(ctx, key, "IN_rival", other).Err()
		}

		err = contended.StoreIngress(ctx, info)

		_, loadErr := rs.LoadIngress(ctx, info.IngressId)
		require.ErrorIs(t, loadErr, service.ErrIngressNotFound, "no attempt could have written the ingress")
		require.ErrorIs(t, err, redis.TxFailedErr, "StoreIngress returned nil although the ingress was never written")
		exists, err := rc.Exists(ctx, service.IngressStatePrefix+info.IngressId).Result()
		require.NoError(t, err)
		require.Zero(t, exists, "a state key was left behind for an ingress that was never written")
	})

	t.Run("UpdateIngressState", func(t *testing.T) {
		require.NoError(t, rs.StoreIngress(ctx, info))

		// An empty payload is a valid zero IngressState: writing it invalidates the
		// WATCH on the state key without tripping the out-of-date checks.
		stateKey := service.IngressStatePrefix + info.IngressId
		rival.write = func(ctx context.Context, key string) error {
			if key != stateKey {
				return nil
			}
			return rc.Set(ctx, key, "", 0).Err()
		}

		err := contended.UpdateIngressState(ctx, info.IngressId, &livekit.IngressState{
			Status: livekit.IngressState_ENDPOINT_PUBLISHING,
		})

		loaded, loadErr := rs.LoadIngress(ctx, info.IngressId)
		require.NoError(t, loadErr)
		require.Equal(t, livekit.IngressState_ENDPOINT_INACTIVE, loaded.State.Status, "no attempt could have written the state")
		require.ErrorIs(t, err, redis.TxFailedErr, "UpdateIngressState returned nil although the state was never written")
	})
}

// RedisStore.UpdateIngress must not recreate an ingress that was deleted after the
// caller loaded it. The existence check runs inside the WATCH on the ingress hash,
// so a delete landing after the check fails the transaction instead.
func TestUpdateIngressDoesNotRecreateDeletedIngress(t *testing.T) {
	ctx := context.Background()
	rc := redisClient(t)
	rs := service.NewRedisStore(rc)

	info := &livekit.IngressInfo{
		IngressId: "IN_deleted",
		StreamKey: "sk_deleted",
		RoomName:  "room_deleted",
	}
	require.NoError(t, rs.StoreIngress(ctx, info))
	t.Cleanup(func() { _ = rs.DeleteIngress(ctx, info) })

	// what an API handler holds when a concurrent DeleteIngress lands
	loaded, err := rs.LoadIngress(ctx, info.IngressId)
	require.NoError(t, err)
	require.NoError(t, rs.DeleteIngress(ctx, info))

	loaded.Name = "renamed"
	require.ErrorIs(t, rs.UpdateIngress(ctx, loaded), service.ErrIngressNotFound)

	_, err = rs.LoadIngress(ctx, info.IngressId)
	require.ErrorIs(t, err, service.ErrIngressNotFound, "the deleted ingress must not reappear")
	_, err = rs.LoadIngressFromStreamKey(ctx, info.StreamKey)
	require.ErrorIs(t, err, service.ErrIngressNotFound, "nor its stream key")
	members, err := rc.SMembers(ctx, service.RoomIngressPrefix+info.RoomName).Result()
	require.NoError(t, err)
	require.Empty(t, members, "nor its room membership")
}

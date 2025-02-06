// Copyright 2023 LiveKit, Inc.
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
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/service"
)

func redisClientDocker(t testing.TB) *redis.Client {
	addr := runRedis(t)
	cli := redis.NewClient(&redis.Options{
		Addr: addr,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := cli.Ping(ctx).Err(); err != nil {
		_ = cli.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cli.Close()
	})
	return cli
}

func redisClient(t testing.TB) *redis.Client {
	cli := redis.NewClient(&redis.Options{
		Addr: "localhost:6379",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := cli.Ping(ctx).Err()
	if err == nil {
		t.Cleanup(func() {
			_ = cli.Close()
		})
		return cli
	}
	_ = cli.Close()
	t.Logf("local redis not available: %v", err)

	t.Logf("starting redis in docker")
	return redisClientDocker(t)
}

func TestIsValidDomain(t *testing.T) {
	list := map[string]bool{
		"turn.myhost.com":  true,
		"turn.google.com":  true,
		"https://host.com": false,
		"turn://host.com":  false,
	}
	for key, result := range list {
		service.IsValidDomain(key)
		require.Equal(t, service.IsValidDomain(key), result)
	}
}

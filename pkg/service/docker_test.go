// Copyright 2024 LiveKit, Inc.
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
	"fmt"
	"log"
	"net"
	"os"
	"testing"
	"time"

	"go.uber.org/atomic"

	mobyclient "github.com/moby/moby/client"
	"github.com/ory/dockertest/v4"
)

var Docker dockertest.ClosablePool

func TestMain(m *testing.M) {
	ctx := context.Background()
	pool, err := dockertest.NewPool(ctx, "")
	if err != nil {
		log.Fatalf("Could not construct pool: %s", err)
	}

	// uses pool to try to connect to Docker
	_, err = pool.Client().Ping(ctx, mobyclient.PingOptions{})
	if err != nil {
		log.Fatalf("Could not connect to Docker: %s", err)
	}
	Docker = pool

	code := m.Run()
	os.Exit(code)
}

func waitTCPPort(t testing.TB, addr string) {
	if err := Docker.Retry(t.Context(), 30*time.Second, func() error {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Log(err)
			return err
		}
		_ = conn.Close()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

var redisLast atomic.Uint32

func runRedis(t testing.TB) string {
	c, err := Docker.Run(t.Context(),
		"redis",
		dockertest.WithName(fmt.Sprintf("lktest-redis-%d", redisLast.Inc())),
		dockertest.WithTag("latest"),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// t.Context() is canceled before cleanup funcs run, so use a
		// non-canceled context to let the container stop/remove complete.
		_ = c.Close(context.Background())
	})
	addr := c.GetHostPort("6379/tcp")
	waitTCPPort(t, addr)

	t.Log("Redis running on", addr)
	return addr
}

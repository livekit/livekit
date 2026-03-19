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

package test

import (
	"fmt"
	"log"
	"net"
	"os"
	"testing"

	"go.uber.org/atomic"

	"github.com/ory/dockertest/v3"
)

var dockerPool *dockertest.Pool

func TestMain(m *testing.M) {
	pool, err := dockertest.NewPool("")
	if err != nil {
		log.Printf("Warning: could not construct Docker pool: %s (multi-node tests will require local Redis)", err)
	} else if err = pool.Client.Ping(); err != nil {
		log.Printf("Warning: could not connect to Docker: %s (multi-node tests will require local Redis)", err)
	} else {
		dockerPool = pool
	}

	// Wire up the Redis address resolver for multi-node tests.
	resolveRedisAddr = redisAddr

	code := m.Run()
	os.Exit(code)
}

func waitTCPPort(t testing.TB, addr string) {
	if err := dockerPool.Retry(func() error {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
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
	if dockerPool == nil {
		t.Fatal("Docker not available, cannot start Redis container")
	}
	c, err := dockerPool.RunWithOptions(&dockertest.RunOptions{
		Name:       fmt.Sprintf("lktest-integ-redis-%d", redisLast.Inc()),
		Repository: "redis", Tag: "latest",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = dockerPool.Purge(c)
	})
	addr := c.GetHostPort("6379/tcp")
	waitTCPPort(t, addr)

	t.Log("Redis running on", addr)
	return addr
}

// redisAddr returns the address of a usable Redis instance.
// It tries localhost:6379 first, and falls back to a Docker container.
func redisAddr(t testing.TB) string {
	conn, err := net.Dial("tcp", "localhost:6379")
	if err == nil {
		_ = conn.Close()
		return "localhost:6379"
	}
	t.Log("local redis not available, starting Docker container")
	return runRedis(t)
}

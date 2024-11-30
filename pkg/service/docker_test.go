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
	"fmt"
	"log"
	"net"
	"os"
	"testing"

	"go.uber.org/atomic"

	"github.com/ory/dockertest/v3"
)

var Docker *dockertest.Pool

func TestMain(m *testing.M) {
	pool, err := dockertest.NewPool("")
	if err != nil {
		log.Fatalf("Could not construct pool: %s", err)
	}

	// uses pool to try to connect to Docker
	err = pool.Client.Ping()
	if err != nil {
		log.Fatalf("Could not connect to Docker: %s", err)
	}
	Docker = pool

	code := m.Run()
	os.Exit(code)
}

func waitTCPPort(t testing.TB, addr string) {
	if err := Docker.Retry(func() error {
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
	c, err := Docker.RunWithOptions(&dockertest.RunOptions{
		Name:       fmt.Sprintf("lktest-redis-%d", redisLast.Inc()),
		Repository: "redis", Tag: "latest",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = Docker.Purge(c)
	})
	addr := c.GetHostPort("6379/tcp")
	waitTCPPort(t, addr)

	t.Log("Redis running on", addr)
	return addr
}

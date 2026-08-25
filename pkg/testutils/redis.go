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

package testutils

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// RedisServer is a redis a test runs for itself rather than sharing the one the
// suite has, so that nothing else can write to it or flush it out from under
// the test.
type RedisServer struct {
	bin  string
	addr string
	dir  string

	cmd    *exec.Cmd
	exited chan struct{}
}

// StartRedis starts a redis on a socket of its own, which nothing else can find
// and which cannot lose a race for a port.
func StartRedis(t *testing.T) *RedisServer {
	// not t.TempDir(), whose name carries the test's own and would put the
	// socket over the length a socket path is allowed to be
	dir, err := os.MkdirTemp("", "lkredis")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	return startRedis(t, filepath.Join(dir, "redis.sock"), dir)
}

func startRedis(t *testing.T, addr, dir string) *RedisServer {
	bin := os.Getenv("REDIS_SERVER")
	if bin == "" {
		bin = "redis-server"
	}
	bin, err := exec.LookPath(bin)
	if err != nil {
		// a test that quietly stops running is worse on CI than one that fails,
		// since nothing there is watching for a run that covered less than the
		// last one did
		if os.Getenv("CI") != "" {
			t.Fatalf("redis-server is not on PATH: %v", err)
		}
		t.Skip("redis-server is not on PATH; set REDIS_SERVER to run this test")
	}

	r := &RedisServer{bin: bin, addr: addr, dir: dir}
	// before starting it, so that a start which fails once the server is running
	// -- on a socket path too long for the OS, say -- does not leave it running
	// for the rest of the suite
	t.Cleanup(r.stop)
	r.start(t)

	return r
}

// Options are what a client needs to reach this redis.
func (r *RedisServer) Options() *redis.Options {
	return &redis.Options{Network: "unix", Addr: r.addr}
}

func (r *RedisServer) start(t *testing.T) {
	// no persistence: nothing here outlives the test
	cmd := exec.Command(r.bin,
		"--port", "0", "--unixsocket", r.addr,
		"--dir", r.dir, "--save", "", "--appendonly", "no")
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Start())

	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()

	// held only once there is a process to hold: stop reads them to mean there
	// is one to kill, and a start that never got that far leaves it nothing
	r.cmd, r.exited = cmd, exited

	r.waitUp(t)
}

func (r *RedisServer) stop() {
	if r.cmd == nil {
		return
	}
	_ = r.cmd.Process.Kill()
	<-r.exited
	r.cmd = nil
}

func (r *RedisServer) waitUp(t *testing.T) {
	c := redis.NewClient(r.Options())
	defer c.Close()

	exited := r.exited
	WithTimeout(t, func() string {
		select {
		case <-exited:
			t.Fatalf("redis-server on %s exited", r.addr)
		default:
		}
		if err := c.Ping(context.Background()).Err(); err != nil {
			return fmt.Sprintf("redis at %s is not up: %v", r.addr, err)
		}
		return ""
	})
}

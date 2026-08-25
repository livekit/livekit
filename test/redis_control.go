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

package test

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/testutils"
)

// controlledRedis is a redis a test can take down and bring back up on a stable
// address, to simulate an outage. It is separate from the redis the rest of the
// suite shares, which is assumed to stay up for the whole run.
type controlledRedis struct {
	bin    string
	port   string
	dir    string
	cmd    *exec.Cmd
	exited chan struct{}
}

// startControlledRedis starts a redis the caller can stop and start again, and
// points the multi-node helpers at it for the rest of the test.
func startControlledRedis(t *testing.T) *controlledRedis {
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

	r := &controlledRedis{bin: bin, port: freePort(t), dir: t.TempDir()}
	// before starting it, so that a start which fails once the server is running
	// does not leave it running for the rest of the suite
	t.Cleanup(r.stop)
	r.start(t)
	useRedisAddr(t, r.addr())
	return r
}

func (r *controlledRedis) addr() string { return net.JoinHostPort("127.0.0.1", r.port) }

func (r *controlledRedis) start(t *testing.T) {
	// no persistence, so a restart comes back empty as the outage in the issue did.
	// bound to the one address rather than to every interface: a wildcard bind is
	// one another server can be started over, since the probe that looks for a
	// free port would find this one's port free
	cmd := exec.Command(r.bin,
		"--port", r.port, "--bind", "127.0.0.1",
		"--save", "", "--appendonly", "no", "--dir", r.dir)
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

func (r *controlledRedis) stop() {
	if r.cmd == nil {
		return
	}
	_ = r.cmd.Process.Kill()
	<-r.exited
	r.cmd = nil
}

func (r *controlledRedis) waitUp(t *testing.T) {
	c := redis.NewClient(&redis.Options{Addr: r.addr()})
	defer c.Close()

	exited := r.exited
	testutils.WithTimeout(t, func() string {
		select {
		case <-exited:
			t.Fatalf("redis-server on port %s exited", r.port)
		default:
		}
		if err := c.Ping(context.Background()).Err(); err != nil {
			return fmt.Sprintf("redis at %s is not up: %v", r.addr(), err)
		}
		return ""
	})
}

// freePort returns a port nothing was listening on a moment ago, from below the
// range the OS hands out to outbound connections: redis has to come back on the
// port it went down on, and an ephemeral one could be taken while it is gone.
// Whoever takes it next wins, so the server started here binds to one address
// rather than to every one, which is what makes a taken port look taken.
func freePort(t *testing.T) string {
	for port := 7900; port < 8000; port++ {
		addr := net.JoinHostPort("127.0.0.1", fmt.Sprint(port))
		l, err := net.Listen("tcp", addr)
		if err != nil {
			continue
		}
		_ = l.Close()
		return fmt.Sprint(port)
	}
	t.Fatal("no free port for redis")
	return ""
}

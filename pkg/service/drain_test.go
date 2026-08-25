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

package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/atomic"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing/routingfakes"
)

const (
	testDrainPoll = 5 * time.Millisecond
	// long enough for the drain to have polled many times over, short enough
	// that a test which expects it to still be waiting does not cost much
	testDrainSettle = 200 * time.Millisecond
	// what a drain that is meant to end gets before the test calls it hung
	testDrainBudget = 5 * time.Second
)

// drainWatch runs a drain the test drives: it says when the participants leave,
// and how long the node has gone without hearing its own keepalive.
type drainWatch struct {
	participants atomic.Bool
	keepalive    atomic.Duration
	ended        chan struct{}
}

func startDrain(t *testing.T, conf config.ShutdownConfig) *drainWatch {
	t.Helper()

	d := &drainWatch{ended: make(chan struct{}, 1)}
	d.participants.Store(true)
	// so that a drain still waiting when its test ends is released rather than
	// left polling for the rest of the run
	t.Cleanup(func() { d.participants.Store(false) })

	go func() {
		waitForDrain(conf, testDrainPoll, d.participants.Load, func() float64 {
			return d.keepalive.Load().Seconds()
		})
		d.ended <- struct{}{}
	}()
	return d
}

func (d *drainWatch) requireFinished(t *testing.T) {
	t.Helper()

	select {
	case <-d.ended:
	case <-time.After(testDrainBudget):
		t.Fatal("the drain never finished")
	}
}

func (d *drainWatch) requireWaiting(t *testing.T) {
	t.Helper()

	select {
	case <-d.ended:
		t.Fatal("the drain ended with participants still on the node")
	case <-time.After(testDrainSettle):
	}
}

// A node with nothing to drain does not wait for a tick to find that out: an
// idle node asked to stop gracefully stops now, not on the next poll.
func TestDrainWithoutParticipantsDoesNotWait(t *testing.T) {
	ended := make(chan struct{}, 1)
	go func() {
		waitForDrain(config.ShutdownConfig{}, time.Hour,
			func() bool { return false }, func() float64 { return 0 })
		ended <- struct{}{}
	}()

	select {
	case <-ended:
	case <-time.After(testDrainBudget):
		t.Fatal("the drain waited for a poll it had no reason to wait for")
	}
}

// The wait is what makes a shutdown graceful. With no deadline configured it is
// still unbounded: a session that has been running for hours is not something
// to cut short on a timer the operator never asked for.
func TestDrainWaitsForParticipantsToLeave(t *testing.T) {
	d := startDrain(t, config.ShutdownConfig{})
	d.requireWaiting(t)

	d.participants.Store(false)
	d.requireFinished(t)
}

// An operator can bound it, and then a node told to shut down eventually does.
func TestDrainTimeoutEndsTheWait(t *testing.T) {
	d := startDrain(t, config.ShutdownConfig{DrainTimeout: 50 * time.Millisecond})

	d.requireFinished(t)
}

// The case from https://github.com/livekit/livekit/issues/4663: a node that
// cannot hear its own keepalive cannot route the signalling its participants
// would leave over, so waiting for them waits for something that cannot happen.
func TestDrainGivesUpWhenTheNodeCannotBeReached(t *testing.T) {
	d := startDrain(t, config.ShutdownConfig{UnreachableDrainTimeout: 50 * time.Millisecond})
	d.requireWaiting(t)

	d.keepalive.Store(time.Second)
	d.requireFinished(t)
}

// A blip is not an outage. The keepalive clock is reset by a ping that makes it
// back, so a node that keeps hearing itself keeps waiting, however long the
// drain takes.
func TestDrainKeepsWaitingWhileTheKeepaliveComesBack(t *testing.T) {
	d := startDrain(t, config.ShutdownConfig{UnreachableDrainTimeout: 50 * time.Millisecond})

	for range 5 {
		d.keepalive.Store(40 * time.Millisecond)
		time.Sleep(20 * time.Millisecond)
		d.keepalive.Store(0)
	}
	d.requireWaiting(t)
}

// Neither deadline applies unless it is configured, so an operator who wants
// today's behavior keeps it.
func TestDrainWithoutDeadlinesWaitsThroughAnOutage(t *testing.T) {
	d := startDrain(t, config.ShutdownConfig{})
	d.keepalive.Store(time.Hour)

	d.requireWaiting(t)
}

// A forced stop is the second SIGTERM, and every test teardown in the suite:
// it has to skip the wait entirely rather than shorten it, since the drain it
// is overriding may be one that could never finish. The room manager is left
// nil to say so -- a wait that ran at all would ask it for its participants.
// The server is set up as a running one, so that the wait is skipped for the
// reason under test and not because there was nothing to stop.
func TestForcedStopDoesNotDrain(t *testing.T) {
	closed := make(chan struct{})
	close(closed)
	s := &LivekitServer{
		config:      &config.Config{Shutdown: config.DefaultShutdownConfig},
		router:      &routingfakes.FakeRouter{},
		currentNode: &routingfakes.FakeLocalNode{},
		doneChan:    make(chan struct{}),
		closedChan:  closed,
	}
	s.running.Store(true)

	stopped := make(chan struct{})
	go func() {
		s.Stop(true)
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(testDrainBudget):
		t.Fatal("a forced stop waited for participants it was told not to wait for")
	}
	fake := s.router.(*routingfakes.FakeRouter)
	require.Equal(t, 1, fake.DrainCallCount(), "a forced stop skipped marking the node as draining")
	require.Equal(t, 1, fake.StopCallCount(), "a forced stop left the router running")
}

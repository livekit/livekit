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
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/twitchtv/twirp"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/psrpc"

	"github.com/livekit/livekit-server/pkg/service"
	"github.com/livekit/livekit-server/pkg/service/servicefakes"
)

// stopLauncher answers each StopEgress with the next of its results.
type stopLauncher struct {
	results []error
	calls   int
}

func (l *stopLauncher) StartEgress(context.Context, *rpc.StartEgressRequest) (*livekit.EgressInfo, error) {
	return nil, errors.New("unused")
}

func (l *stopLauncher) StopEgress(_ context.Context, req *livekit.StopEgressRequest) (*livekit.EgressInfo, error) {
	err := l.results[min(l.calls, len(l.results)-1)]
	l.calls++
	if err != nil {
		return nil, err
	}
	return &livekit.EgressInfo{EgressId: req.EgressId, Status: livekit.EgressStatus_EGRESS_ENDING}, nil
}

func TestStopEgressNoSubscriber(t *testing.T) {
	const retryTimeout = 200 * time.Millisecond
	t.Cleanup(service.SetNoSubscriberRetry(retryTimeout, 10*time.Millisecond))

	ctx := service.WithGrants(context.Background(), &auth.ClaimGrants{Video: &auth.VideoGrant{RoomRecord: true}}, "")
	req := &livekit.StopEgressRequest{EgressId: "EG_test"}
	noSubscriber := psrpc.NewNoSubscriberError(3 * time.Second)

	newService := func(stops []error, statuses ...livekit.EgressStatus) (*service.EgressService, *stopLauncher, *servicefakes.FakeIOClient) {
		launcher := &stopLauncher{results: stops}
		io := &servicefakes.FakeIOClient{}
		for i, status := range statuses {
			io.GetEgressReturnsOnCall(i, &livekit.EgressInfo{EgressId: req.EgressId, Status: status}, nil)
		}
		return service.NewEgressService(nil, launcher, io, nil), launcher, io
	}

	requireTwirpCode := func(t *testing.T, err error, code twirp.ErrorCode) {
		var te twirp.Error
		require.True(t, errors.As(err, &te), "got %v", err)
		require.Equal(t, code, te.Code())
	}

	t.Run("ended egress fails at once", func(t *testing.T) {
		svc, launcher, io := newService([]error{noSubscriber}, livekit.EgressStatus_EGRESS_COMPLETE)

		_, err := svc.StopEgress(ctx, req)
		requireTwirpCode(t, err, twirp.FailedPrecondition)
		require.Equal(t, 1, launcher.calls)
		require.Equal(t, 1, io.GetEgressCallCount())
	})

	t.Run("unknown egress fails at once", func(t *testing.T) {
		svc, launcher, io := newService([]error{noSubscriber})
		io.GetEgressReturns(nil, psrpc.NewErrorf(psrpc.NotFound, "not found"))

		_, err := svc.StopEgress(ctx, req)
		require.ErrorIs(t, err, psrpc.NotFound)
		require.Equal(t, 1, launcher.calls)
	})

	t.Run("live egress is retried until it answers", func(t *testing.T) {
		svc, launcher, _ := newService([]error{noSubscriber, nil}, livekit.EgressStatus_EGRESS_STARTING)

		info, err := svc.StopEgress(ctx, req)
		require.NoError(t, err)
		require.Equal(t, req.EgressId, info.EgressId)
		require.Equal(t, 2, launcher.calls)
	})

	t.Run("live egress is retried until it ends", func(t *testing.T) {
		svc, launcher, _ := newService([]error{noSubscriber},
			livekit.EgressStatus_EGRESS_ACTIVE, livekit.EgressStatus_EGRESS_COMPLETE)

		_, err := svc.StopEgress(ctx, req)
		requireTwirpCode(t, err, twirp.FailedPrecondition)
		require.Equal(t, 2, launcher.calls)
	})

	t.Run("live egress that never answers times out", func(t *testing.T) {
		svc, launcher, io := newService([]error{noSubscriber})
		io.GetEgressReturns(&livekit.EgressInfo{EgressId: req.EgressId, Status: livekit.EgressStatus_EGRESS_ACTIVE}, nil)

		start := time.Now()
		_, err := svc.StopEgress(ctx, req)
		require.ErrorIs(t, err, psrpc.ErrRequestTimedOut)
		require.GreaterOrEqual(t, time.Since(start), retryTimeout)
		require.Greater(t, launcher.calls, 2)
	})

	t.Run("other errors are not retried", func(t *testing.T) {
		svc, launcher, _ := newService([]error{psrpc.ErrRequestTimedOut}, livekit.EgressStatus_EGRESS_ACTIVE)

		_, err := svc.StopEgress(ctx, req)
		require.ErrorIs(t, err, psrpc.ErrRequestTimedOut)
		require.Equal(t, 1, launcher.calls)
	})
}

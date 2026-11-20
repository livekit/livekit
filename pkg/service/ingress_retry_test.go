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
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/psrpc"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/service"
	"github.com/livekit/livekit-server/pkg/service/servicefakes"
	"github.com/livekit/livekit-server/pkg/telemetry/telemetryfakes"
)

// scriptedIngressClient answers each UpdateIngress and DeleteIngress with the
// next of its results.
type scriptedIngressClient struct {
	unusedIngressClient
	results []error
	calls   int
}

func (c *scriptedIngressClient) next() (*livekit.IngressState, error) {
	err := c.results[min(c.calls, len(c.results)-1)]
	c.calls++
	if err != nil {
		return nil, err
	}
	return &livekit.IngressState{}, nil
}

func (c *scriptedIngressClient) UpdateIngress(context.Context, string, *livekit.UpdateIngressRequest, ...psrpc.RequestOption) (*livekit.IngressState, error) {
	return c.next()
}

func (c *scriptedIngressClient) DeleteIngress(context.Context, string, *livekit.DeleteIngressRequest, ...psrpc.RequestOption) (*livekit.IngressState, error) {
	return c.next()
}

func TestActiveIngressNoSubscriber(t *testing.T) {
	const retryTimeout = 200 * time.Millisecond
	t.Cleanup(service.SetNoSubscriberRetry(retryTimeout, 10*time.Millisecond))

	ctx := service.WithGrants(context.Background(), &auth.ClaimGrants{Video: &auth.VideoGrant{IngressAdmin: true}}, "")
	noSubscriber := psrpc.NewNoSubscriberError(3 * time.Second)

	ingress := func(status livekit.IngressState_Status) *livekit.IngressInfo {
		return &livekit.IngressInfo{
			IngressId:           "IN_test",
			StreamKey:           "sk",
			RoomName:            "testroom",
			ParticipantIdentity: "ingress",
			Reusable:            true,
			State:               &livekit.IngressState{Status: status},
		}
	}

	// The handler loads the ingress once, then once more for each retry it considers.
	newService := func(results []error, statuses ...livekit.IngressState_Status) (*service.IngressService, *scriptedIngressClient, *servicefakes.FakeIngressStore) {
		client := &scriptedIngressClient{results: results}
		store := &servicefakes.FakeIngressStore{}
		for i, status := range statuses {
			store.LoadIngressReturnsOnCall(i, ingress(status), nil)
		}
		store.LoadIngressReturns(ingress(statuses[len(statuses)-1]), nil)
		svc := service.NewIngressServiceWithIngressLauncher(
			&config.IngressConfig{},
			"nodeID",
			nil,
			client,
			store,
			&servicefakes.FakeIOClient{},
			&telemetryfakes.FakeTelemetryService{},
			&servicefakes.FakeIngressLauncher{},
		)
		return svc, client, store
	}

	const (
		publishing = livekit.IngressState_ENDPOINT_PUBLISHING
		inactive   = livekit.IngressState_ENDPOINT_INACTIVE
	)

	type call func(*service.IngressService) error
	calls := map[string]call{
		"DeleteIngress": func(svc *service.IngressService) error {
			_, err := svc.DeleteIngress(ctx, &livekit.DeleteIngressRequest{IngressId: "IN_test"})
			return err
		},
		"UpdateIngress": func(svc *service.IngressService) error {
			_, err := svc.UpdateIngress(ctx, &livekit.UpdateIngressRequest{IngressId: "IN_test", Name: "renamed"})
			return err
		},
	}
	stored := func(name string, store *servicefakes.FakeIngressStore) int {
		if name == "DeleteIngress" {
			return store.DeleteIngressCallCount()
		}
		return store.UpdateIngressCallCount()
	}

	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			t.Run("stale state is not retried", func(t *testing.T) {
				svc, client, store := newService([]error{noSubscriber}, publishing, inactive)

				require.NoError(t, call(svc))
				require.Equal(t, 1, client.calls)
				require.Equal(t, 1, stored(name, store))
			})

			t.Run("active ingress is retried until it answers", func(t *testing.T) {
				svc, client, store := newService([]error{noSubscriber, nil}, publishing)

				require.NoError(t, call(svc))
				require.Equal(t, 2, client.calls)
				require.Equal(t, 1, stored(name, store))
			})

			t.Run("active ingress that never answers gives up", func(t *testing.T) {
				svc, client, store := newService([]error{noSubscriber}, publishing)

				start := time.Now()
				require.NoError(t, call(svc), "the RPC error is only logged")
				require.GreaterOrEqual(t, time.Since(start), retryTimeout)
				require.Greater(t, client.calls, 2)
				require.Equal(t, 1, stored(name, store))
			})

			t.Run("other errors are not retried", func(t *testing.T) {
				svc, client, store := newService([]error{psrpc.ErrRequestTimedOut}, publishing)

				require.NoError(t, call(svc))
				require.Equal(t, 1, client.calls)
				require.Equal(t, 1, stored(name, store))
			})
		})
	}
}

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

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/service"
	"github.com/livekit/livekit-server/pkg/service/servicefakes"
)

func TestCreateURLPullIngressScheme(t *testing.T) {
	newService := func(enableUDP bool) (*service.IngressService, *servicefakes.FakeIngressLauncher) {
		launcher := &servicefakes.FakeIngressLauncher{}
		launcher.LaunchPullIngressCalls(func(_ context.Context, info *livekit.IngressInfo) (*livekit.IngressInfo, error) {
			return info, nil
		})

		svc := service.NewIngressServiceWithIngressLauncher(
			&config.IngressConfig{EnableUDPURLPull: enableUDP},
			"nodeID",
			nil,
			nil,
			&servicefakes.FakeIngressStore{},
			nil,
			nil,
			launcher,
		)
		return svc, launcher
	}

	adminCtx := func() context.Context {
		return service.WithGrants(context.Background(), &auth.ClaimGrants{Video: &auth.VideoGrant{IngressAdmin: true}}, "")
	}

	createReq := func(url string) *livekit.CreateIngressRequest {
		return &livekit.CreateIngressRequest{
			InputType:           livekit.IngressInput_URL_INPUT,
			Url:                 url,
			RoomName:            "testroom",
			ParticipantIdentity: "ingress",
		}
	}

	t.Run("udp rejected when disabled", func(t *testing.T) {
		svc, launcher := newService(false)

		_, err := svc.CreateIngress(adminCtx(), createReq("udp://1.2.3.4:1234"))
		require.Error(t, err)
		require.Contains(t, err.Error(), "udp url pull is not enabled")
		require.Zero(t, launcher.LaunchPullIngressCallCount())
	})

	t.Run("udp accepted when enabled", func(t *testing.T) {
		svc, launcher := newService(true)

		info, err := svc.CreateIngress(adminCtx(), createReq("udp://1.2.3.4:1234"))
		require.NoError(t, err)
		require.Equal(t, "udp://1.2.3.4:1234", info.Url)
		require.Equal(t, 1, launcher.LaunchPullIngressCallCount())
	})

	t.Run("other schemes unaffected by the udp option", func(t *testing.T) {
		for _, url := range []string{"http://example.com/live", "https://example.com/live", "srt://1.2.3.4:1234"} {
			svc, _ := newService(false)

			info, err := svc.CreateIngress(adminCtx(), createReq(url))
			require.NoError(t, err, url)
			require.Equal(t, url, info.Url)
		}

		svc, _ := newService(true)
		_, err := svc.CreateIngress(adminCtx(), createReq("rtsp://1.2.3.4/live"))
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid url scheme rtsp")
	})
}

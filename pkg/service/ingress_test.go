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
	"net/netip"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/rpc"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/service"
	"github.com/livekit/livekit-server/pkg/service/servicefakes"
	"github.com/livekit/livekit-server/pkg/telemetry/telemetryfakes"
)

func TestCreateURLPullIngressScheme(t *testing.T) {
	service.SetLookupNetIP(t, func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.215.14")}, nil
	})

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

func TestCreateURLPullIngressHost(t *testing.T) {
	newService := func(allowPrivate bool) (*service.IngressService, *servicefakes.FakeIngressLauncher) {
		launcher := &servicefakes.FakeIngressLauncher{}
		launcher.LaunchPullIngressCalls(func(_ context.Context, info *livekit.IngressInfo) (*livekit.IngressInfo, error) {
			return info, nil
		})

		svc := service.NewIngressServiceWithIngressLauncher(
			&config.IngressConfig{AllowPrivateURLPull: allowPrivate},
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

	ctx := service.WithGrants(context.Background(), &auth.ClaimGrants{Video: &auth.VideoGrant{IngressAdmin: true}}, "")

	create := func(svc *service.IngressService, url string) (*livekit.IngressInfo, error) {
		return svc.CreateIngress(ctx, &livekit.CreateIngressRequest{
			InputType:           livekit.IngressInput_URL_INPUT,
			Url:                 url,
			RoomName:            "testroom",
			ParticipantIdentity: "ingress",
		})
	}

	t.Run("resolved internal addresses rejected", func(t *testing.T) {
		svc, launcher := newService(false)
		for _, url := range []string{
			"http://localhost/live",
			"http://127.0.0.1/live",
			"http://[::1]/live",
			"http://169.254.169.254/latest/meta-data/",
			"http://100.64.0.1/live",
			"http://0.1.2.3/live",
		} {
			_, err := create(svc, url)
			require.Error(t, err, url)
		}
		require.Zero(t, launcher.LaunchPullIngressCallCount())
	})

	service.SetLookupNetIP(t, func(_ context.Context, _, host string) ([]netip.Addr, error) {
		switch strings.ToLower(host) {
		case "public.example", "localhost", "localhost.", "foo.localhost":
			return []netip.Addr{netip.MustParseAddr("93.184.215.14")}, nil
		case "internal.example":
			return []netip.Addr{netip.MustParseAddr("10.1.2.3")}, nil
		case "mixed.example":
			return []netip.Addr{netip.MustParseAddr("93.184.215.14"), netip.MustParseAddr("169.254.169.254")}, nil
		}
		if addr, err := netip.ParseAddr(host); err == nil {
			return []netip.Addr{addr}, nil
		}
		return nil, errors.New("no such host")
	})

	rejected := []string{
		"http://127.0.0.1/live",
		"http://127.1.2.3:8080/live",
		"http://[::1]/live",
		"http://localhost/live",
		"http://LOCALHOST./live",
		"http://foo.localhost/live",
		"http://10.0.0.1/live",
		"http://172.16.5.4/live",
		"https://192.168.1.1/live",
		"http://[fd00::1]/live",
		"http://169.254.169.254/latest/meta-data/",
		"http://[fe80::1]/live",
		"http://100.64.0.1/live",
		"http://0.0.0.0/live",
		"http://0.1.2.3/live",
		"http://[::]/live",
		"http://224.0.0.1/live",
		"http://255.255.255.255/live",
		"http://[::ffff:127.0.0.1]/live",
		"http://[::ffff:10.0.0.1]/live",
		"http://[::ffff:100.64.0.1]/live",
		"http://[::ffff:0.1.2.3]/live",
		"http://198.18.0.1/live",
		"http://198.19.255.254/live",
		"http://240.0.0.1/live",
		"http://[64:ff9b::a9fe:a9fe]/live",
		"http://[64:ff9b::7f00:1]/live",
		"http://[64:ff9b::a00:1]/live",
		"http://[2002:a9fe:a9fe::1]/live",
		"http://[2002:7f00:1::]/live",
		"http://internal.example/live",
		"http://mixed.example/live",
		"http://unresolvable.example/live",
		"srt://127.0.0.1:9000",
		"srt://10.0.0.1:9000?mode=caller",
		"srt://:9000?mode=listener",
	}

	t.Run("internal hosts rejected", func(t *testing.T) {
		svc, launcher := newService(false)
		for _, url := range rejected {
			_, err := create(svc, url)
			require.Error(t, err, url)
		}
		require.Zero(t, launcher.LaunchPullIngressCallCount())
	})

	t.Run("public hosts accepted", func(t *testing.T) {
		for _, url := range []string{
			"http://93.184.215.14/live",
			"https://public.example/live.m3u8",
			"http://[2606:2800:21f:cb07:6820:80da:af6b:8b2c]/live",
			"srt://public.example:9000",
			"http://100.128.0.1/live",
			"http://198.20.0.1/live",
			"http://223.255.255.254/live",
			"http://[64:ff9b::5db8:d70e]/live",
			"http://[2002:5db8:d70e::1]/live",
		} {
			svc, _ := newService(false)
			info, err := create(svc, url)
			require.NoError(t, err, url)
			require.Equal(t, url, info.Url)
		}
	})

	t.Run("internal hosts accepted when allowed", func(t *testing.T) {
		for _, url := range rejected {
			svc, _ := newService(true)
			_, err := create(svc, url)
			require.NoError(t, err, url)
		}
	})
}

// unusedIngressClient is a non-nil rpc.IngressClient for handlers that must not
// reach the ingress service: any call on it panics on the nil embedded interfaces.
type unusedIngressClient struct {
	rpc.IngressInternalClient
	rpc.IngressHandlerClient
}

func (unusedIngressClient) Close() {}

// LoadIngress reads the ingress info and its state from separate keys and
// returns the info with a nil State when the state key is missing (a concurrent
// DeleteIngress, or a create whose state write failed). The API handlers must
// treat that as an empty state instead of panicking, like
// IOInfoService.UpdateIngressState does.
func TestIngressAPIWithNilState(t *testing.T) {
	newService := func() (*service.IngressService, *servicefakes.FakeIngressStore) {
		store := &servicefakes.FakeIngressStore{}
		store.LoadIngressReturns(&livekit.IngressInfo{
			IngressId:           "IN_test",
			StreamKey:           "sk",
			RoomName:            "testroom",
			ParticipantIdentity: "ingress",
			Reusable:            true,
		}, nil)

		svc := service.NewIngressServiceWithIngressLauncher(
			&config.IngressConfig{},
			"nodeID",
			nil,
			unusedIngressClient{},
			store,
			&servicefakes.FakeIOClient{},
			&telemetryfakes.FakeTelemetryService{},
			&servicefakes.FakeIngressLauncher{},
		)
		return svc, store
	}

	adminCtx := service.WithGrants(context.Background(), &auth.ClaimGrants{Video: &auth.VideoGrant{IngressAdmin: true}}, "")

	t.Run("UpdateIngress", func(t *testing.T) {
		svc, store := newService()

		info, err := svc.UpdateIngress(adminCtx, &livekit.UpdateIngressRequest{IngressId: "IN_test", Name: "renamed"})
		require.NoError(t, err)
		require.Equal(t, "renamed", info.Name)
		require.Equal(t, 1, store.UpdateIngressCallCount())
	})

	t.Run("DeleteIngress", func(t *testing.T) {
		svc, store := newService()

		info, err := svc.DeleteIngress(adminCtx, &livekit.DeleteIngressRequest{IngressId: "IN_test"})
		require.NoError(t, err)
		require.Equal(t, livekit.IngressState_ENDPOINT_INACTIVE, info.State.Status)
		require.Equal(t, 1, store.DeleteIngressCallCount())
	})
}

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
	"github.com/twitchtv/twirp"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/service"
)

func TestCreateRoomWebhooks(t *testing.T) {
	newAllocator := func(t *testing.T, mut func(*config.Config)) (service.RoomAllocator, *config.Config) {
		conf, err := config.NewConfig("", true, nil, nil)
		require.NoError(t, err)
		if mut != nil {
			mut(conf)
		}
		node, err := routing.NewLocalNode(conf)
		require.NoError(t, err)
		ra, conf := newTestRoomAllocator(t, conf, node.Clone())
		return ra, conf
	}

	reqWebhook := &livekit.WebhookConfig{Url: "https://example.com/req"}
	presetWebhook := &livekit.WebhookConfig{Url: "https://example.com/preset"}

	t.Run("webhooks on the request land on RoomInternal", func(t *testing.T) {
		ra, _ := newAllocator(t, nil)

		_, internal, _, err := ra.CreateRoom(context.Background(), &livekit.CreateRoomRequest{
			Name:     "myroom",
			Webhooks: []*livekit.WebhookConfig{reqWebhook},
		}, true)
		require.NoError(t, err)
		require.Len(t, internal.GetWebhooks(), 1)
		require.Equal(t, reqWebhook.Url, internal.GetWebhooks()[0].Url)
	})

	t.Run("a room with no webhooks configured gets none", func(t *testing.T) {
		ra, _ := newAllocator(t, nil)

		_, internal, _, err := ra.CreateRoom(context.Background(), &livekit.CreateRoomRequest{Name: "myroom"}, true)
		require.NoError(t, err)
		require.Empty(t, internal.GetWebhooks())
	})

	t.Run("preset webhooks apply when the request has none", func(t *testing.T) {
		ra, _ := newAllocator(t, func(conf *config.Config) {
			conf.Room.RoomConfigurations = map[string]*livekit.RoomConfiguration{
				"support": {Webhooks: []*livekit.WebhookConfig{presetWebhook}},
			}
		})

		_, internal, _, err := ra.CreateRoom(context.Background(), &livekit.CreateRoomRequest{
			Name:       "myroom",
			RoomPreset: "support",
		}, true)
		require.NoError(t, err)
		require.Len(t, internal.GetWebhooks(), 1)
		require.Equal(t, presetWebhook.Url, internal.GetWebhooks()[0].Url)
	})

	t.Run("request webhooks win over the preset", func(t *testing.T) {
		ra, _ := newAllocator(t, func(conf *config.Config) {
			conf.Room.RoomConfigurations = map[string]*livekit.RoomConfiguration{
				"support": {Webhooks: []*livekit.WebhookConfig{presetWebhook}},
			}
		})

		_, internal, _, err := ra.CreateRoom(context.Background(), &livekit.CreateRoomRequest{
			Name:       "myroom",
			RoomPreset: "support",
			Webhooks:   []*livekit.WebhookConfig{reqWebhook},
		}, true)
		require.NoError(t, err)
		require.Len(t, internal.GetWebhooks(), 1)
		require.Equal(t, reqWebhook.Url, internal.GetWebhooks()[0].Url)
	})
}

func TestCreateRoomWebhookValidation(t *testing.T) {
	createCtx := func() context.Context {
		return service.WithGrants(context.Background(), &auth.ClaimGrants{Video: &auth.VideoGrant{RoomCreate: true}}, "")
	}
	requireInvalidArg := func(t *testing.T, err error) {
		t.Helper()
		terr, ok := err.(twirp.Error)
		require.True(t, ok, "expected twirp error, got %T (%v)", err, err)
		require.Equal(t, twirp.InvalidArgument, terr.Code())
	}

	// newTestRoomService registers "APIkey" with the key provider
	for _, tc := range []struct {
		name string
		wh   *livekit.WebhookConfig
	}{
		{"empty url", &livekit.WebhookConfig{}},
		{"relative url", &livekit.WebhookConfig{Url: "/hook"}},
		{"non-http scheme", &livekit.WebhookConfig{Url: "ftp://example.com/hook"}},
		{"no host", &livekit.WebhookConfig{Url: "https:///hook"}},
		{"unknown signing key", &livekit.WebhookConfig{Url: "https://example.com/hook", SigningKey: "nope"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := newTestRoomService(config.LimitConfig{})
			_, err := svc.CreateRoom(createCtx(), &livekit.CreateRoomRequest{
				Name:     "myroom",
				Webhooks: []*livekit.WebhookConfig{tc.wh},
			})
			requireInvalidArg(t, err)
		})
	}

	t.Run("valid webhook is accepted", func(t *testing.T) {
		svc := newTestRoomService(config.LimitConfig{})
		_, err := svc.CreateRoom(createCtx(), &livekit.CreateRoomRequest{
			Name: "myroom",
			Webhooks: []*livekit.WebhookConfig{
				{Url: "https://example.com/hook", SigningKey: "APIkey"},
				{Url: "http://example.com/hook2"}, // empty signing key means the default
			},
		})
		require.NoError(t, err)
	})
}

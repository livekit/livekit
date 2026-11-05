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

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/protocol/utils/guid"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/pkg/service"
)

func newTestSIPService(t *testing.T) (context.Context, *service.SIPService, *service.RedisStore) {
	rs := redisStoreDocker(t)
	s := service.NewSIPService(nil, "", nil, nil, rs, nil, nil)
	ctx := service.WithGrants(context.Background(), &auth.ClaimGrants{SIP: &auth.SIPGrant{Admin: true}}, "")
	return ctx, s, rs
}

func TestSIPInboundTrunkNumbersNormalized(t *testing.T) {
	ctx, s, rs := newTestSIPService(t)

	created, err := s.CreateSIPInboundTrunk(ctx, &livekit.CreateSIPInboundTrunkRequest{
		Trunk: &livekit.SIPInboundTrunkInfo{
			Name:    "create",
			Numbers: []string{"15550001001", "+1 (555) 000-1002", "support", "+15550001001"},
		},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"+15550001001", "+15550001002", "support"}, created.Numbers)

	stored, err := rs.LoadSIPInboundTrunk(ctx, created.SipTrunkId)
	require.NoError(t, err)
	require.Equal(t, created.Numbers, stored.Numbers)

	update := func(t *testing.T, id string, u *livekit.SIPInboundTrunkUpdate) *livekit.SIPInboundTrunkInfo {
		info, err := s.UpdateSIPInboundTrunk(ctx, &livekit.UpdateSIPInboundTrunkRequest{
			SipTrunkId: id,
			Action:     &livekit.UpdateSIPInboundTrunkRequest_Update{Update: u},
		})
		require.NoError(t, err)
		stored, err := rs.LoadSIPInboundTrunk(ctx, id)
		require.NoError(t, err)
		require.Equal(t, info.Numbers, stored.Numbers)
		return info
	}

	t.Run("remove unnormalized", func(t *testing.T) {
		info := update(t, created.SipTrunkId, &livekit.SIPInboundTrunkUpdate{
			Numbers: &livekit.ListUpdate{Remove: []string{"15550001001"}},
		})
		require.Equal(t, []string{"+15550001002", "support"}, info.Numbers)
	})

	t.Run("add unnormalized", func(t *testing.T) {
		info := update(t, created.SipTrunkId, &livekit.SIPInboundTrunkUpdate{
			Numbers: &livekit.ListUpdate{Add: []string{"15550001003", "15550001002"}},
		})
		require.Equal(t, []string{"+15550001002", "support", "+15550001003"}, info.Numbers)
	})

	t.Run("replace", func(t *testing.T) {
		info, err := s.UpdateSIPInboundTrunk(ctx, &livekit.UpdateSIPInboundTrunkRequest{
			SipTrunkId: created.SipTrunkId,
			Action: &livekit.UpdateSIPInboundTrunkRequest_Replace{Replace: &livekit.SIPInboundTrunkInfo{
				Name:    "replace",
				Numbers: []string{"15550001004"},
			}},
		})
		require.NoError(t, err)
		require.Equal(t, []string{"+15550001004"}, info.Numbers)
	})

	t.Run("legacy row normalized on edit", func(t *testing.T) {
		id := guid.New(utils.SIPTrunkPrefix)
		require.NoError(t, rs.StoreSIPInboundTrunk(ctx, &livekit.SIPInboundTrunkInfo{
			SipTrunkId: id,
			Name:       "legacy",
			Numbers:    []string{"15550001005", "+15550001005"},
		}))
		info := update(t, id, &livekit.SIPInboundTrunkUpdate{Name: proto.String("renamed")})
		require.Equal(t, []string{"+15550001005"}, info.Numbers)
	})
}

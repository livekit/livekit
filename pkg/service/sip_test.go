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
	"github.com/livekit/psrpc"
	"github.com/stretchr/testify/require"
	"github.com/twitchtv/twirp"
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

func requireInvalidArgument(t *testing.T, err error) {
	t.Helper()
	var terr twirp.Error
	require.ErrorAs(t, err, &terr)
	require.Equal(t, twirp.InvalidArgument, terr.Code())
}

func TestSIPInboundTrunkRejectsURINumbers(t *testing.T) {
	ctx, s, rs := newTestSIPService(t)

	create := func(nums ...string) (*livekit.SIPInboundTrunkInfo, error) {
		return s.CreateSIPInboundTrunk(ctx, &livekit.CreateSIPInboundTrunkRequest{
			Trunk: &livekit.SIPInboundTrunkInfo{Name: "trunk", Numbers: nums},
		})
	}
	update := func(id string, u *livekit.SIPInboundTrunkUpdate) (*livekit.SIPInboundTrunkInfo, error) {
		return s.UpdateSIPInboundTrunk(ctx, &livekit.UpdateSIPInboundTrunkRequest{
			SipTrunkId: id,
			Action:     &livekit.UpdateSIPInboundTrunkRequest_Update{Update: u},
		})
	}

	t.Run("create", func(t *testing.T) {
		for _, num := range []string{
			"sip:support@example.com",
			"SIPS:support@example.com",
			"sip:support",
			"tel:+15550001001",
			"support@example.com",
		} {
			_, err := create("+15550001001", num)
			requireInvalidArgument(t, err)
		}
		_, err := create("+15550002001", "support")
		require.NoError(t, err)
	})

	t.Run("update", func(t *testing.T) {
		const legacyURI = "sip:legacy@example.com"
		id := guid.New(utils.SIPTrunkPrefix)
		require.NoError(t, rs.StoreSIPInboundTrunk(ctx, &livekit.SIPInboundTrunkInfo{
			SipTrunkId: id,
			Name:       "legacy",
			Numbers:    []string{"+15550003001", legacyURI},
		}))

		info, err := update(id, &livekit.SIPInboundTrunkUpdate{Name: proto.String("renamed")})
		require.NoError(t, err)
		require.Equal(t, []string{"+15550003001", legacyURI}, info.Numbers)

		info, err = update(id, &livekit.SIPInboundTrunkUpdate{
			Numbers: &livekit.ListUpdate{Add: []string{"+15550003002"}},
		})
		require.NoError(t, err)
		require.Equal(t, []string{"+15550003001", legacyURI, "+15550003002"}, info.Numbers)

		_, err = update(id, &livekit.SIPInboundTrunkUpdate{
			Numbers: &livekit.ListUpdate{Add: []string{"sip:new@example.com"}},
		})
		requireInvalidArgument(t, err)

		_, err = s.UpdateSIPInboundTrunk(ctx, &livekit.UpdateSIPInboundTrunkRequest{
			SipTrunkId: id,
			Action: &livekit.UpdateSIPInboundTrunkRequest_Replace{Replace: &livekit.SIPInboundTrunkInfo{
				Name:    "replace",
				Numbers: []string{legacyURI, "sip:new@example.com"},
			}},
		})
		requireInvalidArgument(t, err)

		info, err = update(id, &livekit.SIPInboundTrunkUpdate{
			Numbers: &livekit.ListUpdate{Remove: []string{legacyURI}},
		})
		require.NoError(t, err)
		require.Equal(t, []string{"+15550003001", "+15550003002"}, info.Numbers)

		_, err = update(id, &livekit.SIPInboundTrunkUpdate{
			Numbers: &livekit.ListUpdate{Add: []string{legacyURI}},
		})
		requireInvalidArgument(t, err)
	})
}

func TestCreateSIPParticipantSourceCallUnsupported(t *testing.T) {
	req := &livekit.CreateSIPParticipantRequest{SipSourceCallId: "SCL_1"}

	t.Run("without SIP call permission", func(t *testing.T) {
		_, err := (&service.SIPService{}).CreateSIPParticipant(context.Background(), req)
		var terr twirp.Error
		require.ErrorAs(t, err, &terr)
		require.Equal(t, twirp.Unauthenticated, terr.Code())
	})

	t.Run("with SIP call permission", func(t *testing.T) {
		ctx := service.WithGrants(context.Background(), &auth.ClaimGrants{SIP: &auth.SIPGrant{Call: true}}, "")
		_, err := (&service.SIPService{}).CreateSIPParticipant(ctx, req)
		code, _ := psrpc.GetErrorCode(err)
		require.Equal(t, psrpc.Unimplemented, code)
	})
}

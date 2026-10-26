// Copyright 2024 LiveKit, Inc.
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
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

// stubRoomAllocator is a RoomAllocator whose ValidateCreateRoom always succeeds. The embedded
// interface is nil, so any other method panics, which keeps the tests honest about what the
// join path actually calls.
type stubRoomAllocator struct{ RoomAllocator }

func (stubRoomAllocator) ValidateCreateRoom(context.Context, livekit.RoomName) error { return nil }

func grantRequest(grant *auth.ClaimGrants) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	return r.WithContext(WithGrants(r.Context(), grant, "apikey"))
}

// TestConnectLimits checks that both HTTP join paths (WHIP ingest and the signal connection via
// ValidateConnectRequest) reject the same over-limit tokens through checkConnectLimits.
func TestConnectLimits(t *testing.T) {
	baseGrant := func() *auth.ClaimGrants {
		return &auth.ClaimGrants{
			Identity: "id",
			Name:     "name",
			Video:    &auth.VideoGrant{RoomJoin: true, Room: "room"},
		}
	}

	cases := []struct {
		name   string
		limit  config.LimitConfig
		mutate func(*auth.ClaimGrants)
		err    error
	}{
		{
			name:   "identity exceeds",
			limit:  config.LimitConfig{MaxParticipantIdentityLength: 5},
			mutate: func(g *auth.ClaimGrants) { g.Identity = "toolongidentity" },
			err:    ErrParticipantIdentityExceedsLimits,
		},
		{
			name:   "room name exceeds",
			limit:  config.LimitConfig{MaxRoomNameLength: 3},
			mutate: func(g *auth.ClaimGrants) { g.Video.Room = "toolongroom" },
			err:    ErrRoomNameExceedsLimits,
		},
		{
			name:   "name exceeds",
			limit:  config.LimitConfig{MaxParticipantNameLength: 5},
			mutate: func(g *auth.ClaimGrants) { g.Name = "toolongname" },
			err:    ErrNameExceedsLimits,
		},
		{
			name:   "metadata exceeds",
			limit:  config.LimitConfig{MaxMetadataSize: 5},
			mutate: func(g *auth.ClaimGrants) { g.Metadata = "abcdefg" },
			err:    ErrMetadataExceedsLimits,
		},
		{
			name:   "attributes exceeds",
			limit:  config.LimitConfig{MaxAttributesSize: 5},
			mutate: func(g *auth.ClaimGrants) { g.Attributes = map[string]string{"k": "abcdefg"} },
			err:    ErrAttributeExceedsLimits,
		},
	}

	for _, tc := range cases {
		t.Run("WHIP/"+tc.name, func(t *testing.T) {
			g := baseGrant()
			tc.mutate(g)
			svc := &WHIPService{config: &config.Config{Limit: tc.limit}, roomAllocator: stubRoomAllocator{}}
			_, code, err := svc.validateCreate(httptest.NewRecorder(), grantRequest(g))
			require.ErrorIs(t, err, tc.err)
			require.Equal(t, http.StatusBadRequest, code)
		})

		t.Run("signal/"+tc.name, func(t *testing.T) {
			g := baseGrant()
			tc.mutate(g)
			// nil router skips the node-limit check, reaching the grant checks
			_, code, err := ValidateConnectRequest(logger.GetLogger(), grantRequest(g), tc.limit, ValidateConnectRequestParams{}, nil, stubRoomAllocator{})
			require.ErrorIs(t, err, tc.err)
			require.Equal(t, http.StatusBadRequest, code)
		})
	}

	t.Run("signal/identity exceeds after publish suffix", func(t *testing.T) {
		g := baseGrant()
		g.Video.SetCanPublish(true)
		// "id" fits a limit of 5, but the final identity "id#abcd" does not
		params := ValidateConnectRequestParams{publish: "abcd"}
		_, code, err := ValidateConnectRequest(logger.GetLogger(), grantRequest(g), config.LimitConfig{MaxParticipantIdentityLength: 5}, params, nil, stubRoomAllocator{})
		require.ErrorIs(t, err, ErrParticipantIdentityExceedsLimits)
		require.Equal(t, http.StatusBadRequest, code)
	})

	t.Run("within limits", func(t *testing.T) {
		limit := config.LimitConfig{MaxParticipantIdentityLength: 5, MaxRoomNameLength: 10, MaxParticipantNameLength: 5, MaxMetadataSize: 5, MaxAttributesSize: 5}
		_, code, err := ValidateConnectRequest(logger.GetLogger(), grantRequest(baseGrant()), limit, ValidateConnectRequestParams{}, nil, stubRoomAllocator{})
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, code)
	})
}

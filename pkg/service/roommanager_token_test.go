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

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/auth"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/rtc/types/typesfakes"
)

func TestRefreshTokenSigningKey(t *testing.T) {
	keys := map[string]string{
		"key1": "secret1-secret1-secret1-secret1-secret1",
		"key2": "secret2-secret2-secret2-secret2-secret2",
	}
	r := &RoomManager{config: &config.Config{Keys: keys}}

	refresh := func(t *testing.T, apiKey string) *auth.APIKeyTokenVerifier {
		p := &typesfakes.FakeLocalParticipant{}
		p.APIKeyReturns(apiKey)
		p.ClaimGrantsReturns(&auth.ClaimGrants{Video: &auth.VideoGrant{RoomJoin: true}})
		require.NoError(t, r.refreshToken(p))
		require.Equal(t, 1, p.SendRefreshTokenCallCount())

		v, err := auth.ParseAPIToken(p.SendRefreshTokenArgsForCall(0))
		require.NoError(t, err)
		_, _, err = v.Verify(keys[v.APIKey()])
		require.NoError(t, err)
		return v
	}

	t.Run("join key", func(t *testing.T) {
		for apiKey := range keys {
			require.Equal(t, apiKey, refresh(t, apiKey).APIKey())
		}
	})

	t.Run("unknown key falls back", func(t *testing.T) {
		require.Contains(t, keys, refresh(t, "unknown").APIKey())
	})
}

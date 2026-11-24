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
	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/rtc/types/typesfakes"
	sutils "github.com/livekit/livekit-server/pkg/utils"
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

func TestGetIceConfigClientPreference(t *testing.T) {
	const (
		none = livekit.ICECandidateType_ICT_NONE
		tcp  = livekit.ICECandidateType_ICT_TCP
		tls  = livekit.ICECandidateType_ICT_TLS
	)
	turnsConf := &config.Config{}
	turnsConf.TURN.Enabled = true
	turnsConf.TURN.TLSPort = 5349

	udp, tcpPref, tlsPref := livekit.CandidateProtocol_UDP, livekit.CandidateProtocol_TCP, livekit.CandidateProtocol_TLS
	cases := []struct {
		name    string
		conf    *config.Config
		cached  livekit.ICECandidateType
		clients []livekit.CandidateProtocol
		want    livekit.ICECandidateType
	}{
		{"no preference", turnsConf, none, []livekit.CandidateProtocol{udp}, none},
		{"client tcp", turnsConf, none, []livekit.CandidateProtocol{tcpPref}, tcp},
		{"client tls", turnsConf, none, []livekit.CandidateProtocol{tlsPref}, tls},
		{"client tls without turns", &config.Config{}, none, []livekit.CandidateProtocol{tlsPref}, tcp},
		{"cached tls beats client tcp", turnsConf, tls, []livekit.CandidateProtocol{tcpPref}, tls},
		{"client tls beats cached tcp", turnsConf, tcp, []livekit.CandidateProtocol{tlsPref}, tls},
		{"unknown value ignored", turnsConf, none, []livekit.CandidateProtocol{7}, none},
		{"resume without preference keeps join preference", turnsConf, none, []livekit.CandidateProtocol{udp, tcpPref}, tcp},
		{"resume takes stronger of request and join", turnsConf, none, []livekit.CandidateProtocol{tlsPref, tcpPref}, tls},
		{"unknown resume value keeps join preference", turnsConf, none, []livekit.CandidateProtocol{7, tcpPref}, tcp},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &RoomManager{config: c.conf, iceConfigCache: sutils.NewIceConfigCache[iceConfigCacheKey](0)}
			t.Cleanup(r.iceConfigCache.Stop)
			p := &typesfakes.FakeLocalParticipant{}
			p.IdentityReturns("p")
			if c.cached != none {
				r.iceConfigCache.Put(iceConfigCacheKey{"room", "p"}, &livekit.ICEConfig{PreferenceSubscriber: c.cached, PreferencePublisher: c.cached})
			}

			ic := r.getIceConfig("room", p, c.clients...)
			require.Equal(t, c.want, ic.PreferenceSubscriber)
			require.Equal(t, c.want, ic.PreferencePublisher)
		})
	}
}

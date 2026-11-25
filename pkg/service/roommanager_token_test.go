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
	"google.golang.org/protobuf/proto"

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
	fullConf := &config.Config{}
	fullConf.RTC.TCPPort = 7881
	fullConf.TURN.Enabled = true
	fullConf.TURN.TLSPort = 5349
	tcpOnlyConf := &config.Config{}
	tcpOnlyConf.RTC.TCPPort = 7881
	udpOnlyConf := &config.Config{}

	cases := []struct {
		name   string
		conf   *config.Config
		cached livekit.ICECandidateType
		client livekit.CandidateProtocol
		want   livekit.ICECandidateType
	}{
		{"no preference", fullConf, none, livekit.CandidateProtocol_UDP, none},
		{"client tcp", fullConf, none, livekit.CandidateProtocol_TCP, tcp},
		{"client tls", fullConf, none, livekit.CandidateProtocol_TLS, tls},
		{"client tls without turns", tcpOnlyConf, none, livekit.CandidateProtocol_TLS, tcp},
		{"client tcp without ice/tcp", udpOnlyConf, none, livekit.CandidateProtocol_TCP, none},
		{"client tls without turns or ice/tcp", udpOnlyConf, none, livekit.CandidateProtocol_TLS, none},
		{"cached tls beats client tcp", fullConf, tls, livekit.CandidateProtocol_TCP, tls},
		{"client tls beats cached tcp", fullConf, tcp, livekit.CandidateProtocol_TLS, tls},
		{"unknown value ignored", fullConf, none, livekit.CandidateProtocol(7), none},
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

			ic := r.getIceConfig("room", p, c.client)
			require.Equal(t, c.want, ic.PreferenceSubscriber)
			require.Equal(t, c.want, ic.PreferencePublisher)
		})
	}
}

func TestIceServersForParticipantTLSOnly(t *testing.T) {
	conf := &config.Config{}
	conf.RTC.TURNServers = []config.TURNServer{
		{Host: "udp.example.com", Port: 3478, Protocol: "udp"},
		{Host: "tcp.example.com", Port: 3478, Protocol: "tcp"},
		{Host: "tls.example.com", Port: 443, Protocol: "tls"},
	}
	r := &RoomManager{config: conf}
	p := &typesfakes.FakeLocalParticipant{}
	p.IDReturns("PA_test")

	urls := func(tlsOnly bool) []string {
		var out []string
		for _, s := range r.iceServersForParticipant("key", p, tlsOnly) {
			out = append(out, s.Urls...)
		}
		return out
	}

	require.Equal(t, []string{
		"turn:udp.example.com:3478?transport=udp",
		"turn:tcp.example.com:3478?transport=tcp",
		"turns:tls.example.com:443?transport=tcp",
	}, urls(false))
	require.Equal(t, []string{"turns:tls.example.com:443?transport=tcp"}, urls(true))
}

func TestResumeICEUsesLearnedFallback(t *testing.T) {
	conf := &config.Config{}
	conf.RTC.TURNServers = []config.TURNServer{
		{Host: "udp.example.com", Port: 3478, Protocol: "udp"},
		{Host: "tls.example.com", Port: 443, Protocol: "tls"},
	}
	r := &RoomManager{config: conf, iceConfigCache: sutils.NewIceConfigCache[iceConfigCacheKey](0)}
	t.Cleanup(r.iceConfigCache.Stop)
	p := &typesfakes.FakeLocalParticipant{}
	p.IdentityReturns("p")
	p.IDReturns("PA_test")

	urls := func(iceServers []*livekit.ICEServer) []string {
		var out []string
		for _, s := range iceServers {
			out = append(out, s.Urls...)
		}
		return out
	}

	iceConfig, iceServers := r.resumeICE("room", p, "key", livekit.CandidateProtocol_UDP)
	require.Equal(t, livekit.ICECandidateType_ICT_NONE, iceConfig.PreferenceSubscriber)
	require.Equal(t, []string{
		"turn:udp.example.com:3478?transport=udp",
		"turns:tls.example.com:443?transport=tcp",
	}, urls(iceServers))

	// the participant reports a TLS fallback that it learned from a failure
	tls := &livekit.ICEConfig{PreferenceSubscriber: livekit.ICECandidateType_ICT_TLS, PreferencePublisher: livekit.ICECandidateType_ICT_TLS}
	r.iceConfigCache.Put(iceConfigCacheKey{"room", "p"}, tls)

	iceConfig, iceServers = r.resumeICE("room", p, "key", livekit.CandidateProtocol_UDP)
	require.True(t, proto.Equal(tls, iceConfig))
	require.True(t, proto.Equal(tls, p.SetICEConfigArgsForCall(1)))
	require.Equal(t, []string{"turns:tls.example.com:443?transport=tcp"}, urls(iceServers))
}

func TestResumeCandidateProtocol(t *testing.T) {
	udp, tcp, tls := livekit.CandidateProtocol_UDP, livekit.CandidateProtocol_TCP, livekit.CandidateProtocol_TLS
	require.Equal(t, tls, resumeCandidateProtocol(udp, tls), "no preference on resume keeps join")
	require.Equal(t, tcp, resumeCandidateProtocol(tcp, tls), "tcp on resume replaces tls from join")
	require.Equal(t, tls, resumeCandidateProtocol(tls, udp), "tls on resume replaces no preference")
	require.Equal(t, udp, resumeCandidateProtocol(udp, udp))
}

func TestResumeUnsupportedPreferenceKeepsJoin(t *testing.T) {
	// TURN/TLS configured, ICE/TCP off
	conf := &config.Config{}
	conf.TURN.Enabled = true
	conf.TURN.TLSPort = 5349
	r := &RoomManager{config: conf, iceConfigCache: sutils.NewIceConfigCache[iceConfigCacheKey](0)}
	t.Cleanup(r.iceConfigCache.Stop)
	p := &typesfakes.FakeLocalParticipant{}
	p.IdentityReturns("p")

	cp := resumeCandidateProtocol(r.supportedCandidateProtocol(livekit.CandidateProtocol_TCP), livekit.CandidateProtocol_TLS)
	require.Equal(t, livekit.ICECandidateType_ICT_TLS, r.getIceConfig("room", p, cp).PreferenceSubscriber)
}

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

package rtc

import (
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/mediatransportutil/pkg/rtcconfig"
)

// catchAlls returns the two family-scoped trailing rules (v4, v6).
func catchAlls(t *testing.T, rules []webrtc.ICEAddressRewriteRule) (v4, v6 webrtc.ICEAddressRewriteRule) {
	t.Helper()
	require.GreaterOrEqual(t, len(rules), 2)
	v4, v6 = rules[len(rules)-2], rules[len(rules)-1]
	require.Empty(t, v4.Local)
	require.Empty(t, v6.Local)
	require.Equal(t, ipv4Networks, v4.Networks)
	require.Equal(t, ipv6Networks, v6.Networks)
	require.Equal(t, webrtc.ICEAddressRewriteReplace, v4.Mode)
	require.Equal(t, webrtc.ICEAddressRewriteReplace, v6.Mode)
	return v4, v6
}

func TestBuildAdvertisedIPRules(t *testing.T) {
	localIPs := []string{"10.0.0.5", "192.168.1.10"}

	t.Run("pure external list produces a v4 catch-all and a v6 drop", func(t *testing.T) {
		rules, err := buildAdvertisedIPRules([]string{"203.0.113.1", "203.0.113.2"}, localIPs, false)
		require.NoError(t, err)
		require.Len(t, rules, 2)
		v4, v6 := catchAlls(t, rules)
		require.Equal(t, []string{"203.0.113.1", "203.0.113.2"}, v4.External)
		require.Equal(t, webrtc.ICECandidateTypeHost, v4.AsCandidateType)
		require.Equal(t, []string{dropSentinelForIPv6Rule}, v6.External)
	})

	t.Run("local and external mix advertises both, suppresses unlisted locals", func(t *testing.T) {
		rules, err := buildAdvertisedIPRules([]string{"203.0.113.1", "10.0.0.5"}, localIPs, false)
		require.NoError(t, err)
		require.Len(t, rules, 3) // identity + v4 catch-all + v6 drop

		// identity rule shields the listed local from the catch-all and also
		// carries the NAT-side address, so it is advertised even when this is the
		// only socket on the host
		require.Equal(t, []string{"10.0.0.5", "203.0.113.1"}, rules[0].External)
		require.Equal(t, "10.0.0.5", rules[0].Local)
		require.Equal(t, webrtc.ICEAddressRewriteReplace, rules[0].Mode)

		// the catch-all rewrites every other local (e.g. 192.168.1.10) to the
		// NAT-side address ONLY — never to the interface-scoped 10.0.0.5, whose
		// port may differ under port-range gathering
		v4, _ := catchAlls(t, rules)
		require.Equal(t, []string{"203.0.113.1"}, v4.External)
	})

	t.Run("two local IPs advertise themselves only; unlisted locals are dropped", func(t *testing.T) {
		rules, err := buildAdvertisedIPRules([]string{"10.0.0.5", "192.168.1.10"}, []string{"10.0.0.5", "192.168.1.10", "172.17.0.1"}, false)
		require.NoError(t, err)
		require.Len(t, rules, 4) // 2 identity + v4 drop + v6 drop
		require.Equal(t, "10.0.0.5", rules[0].Local)
		require.Equal(t, []string{"10.0.0.5"}, rules[0].External)
		require.Equal(t, "192.168.1.10", rules[1].Local)
		require.Equal(t, []string{"192.168.1.10"}, rules[1].External)
		// nothing NAT-side to advertise for 172.17.0.1: drop rule, not a catch-all
		v4, v6 := catchAlls(t, rules)
		require.Equal(t, []string{dropSentinelForIPv4Rule}, v4.External)
		require.Equal(t, []string{dropSentinelForIPv6Rule}, v6.External)
	})

	t.Run("external/local pair replaces the local and stays out of the catch-all", func(t *testing.T) {
		rules, err := buildAdvertisedIPRules([]string{"203.0.113.1/10.0.0.5"}, localIPs, false)
		require.NoError(t, err)
		require.Len(t, rules, 3) // pair + v4 drop + v6 drop
		require.Equal(t, "10.0.0.5", rules[0].Local)
		require.Equal(t, []string{"203.0.113.1"}, rules[0].External)
		require.Equal(t, webrtc.ICEAddressRewriteReplace, rules[0].Mode)
		// 203.0.113.1 DNATs to 10.0.0.5's socket only; 192.168.1.10's socket must
		// not be advertised as 203.0.113.1 (its port has no listener there)
		v4, _ := catchAlls(t, rules)
		require.Equal(t, []string{dropSentinelForIPv4Rule}, v4.External)
	})

	t.Run("pair local also listed bare advertises both", func(t *testing.T) {
		rules, err := buildAdvertisedIPRules([]string{"203.0.113.1/10.0.0.5", "10.0.0.5"}, localIPs, false)
		require.NoError(t, err)
		require.Len(t, rules, 3)
		require.Equal(t, "10.0.0.5", rules[0].Local)
		require.ElementsMatch(t, []string{"203.0.113.1", "10.0.0.5"}, rules[0].External)
	})

	t.Run("multiple pairs for the same local merge into one rule", func(t *testing.T) {
		rules, err := buildAdvertisedIPRules([]string{"203.0.113.1/10.0.0.5", "203.0.113.2/10.0.0.5"}, localIPs, false)
		require.NoError(t, err)
		require.Len(t, rules, 3)
		require.Equal(t, "10.0.0.5", rules[0].Local)
		require.Equal(t, []string{"203.0.113.1", "203.0.113.2"}, rules[0].External)
	})

	t.Run("bare non-local plus pair: pair rule gains the NAT-side address, catch-all carries only it", func(t *testing.T) {
		rules, err := buildAdvertisedIPRules([]string{"198.51.100.7", "203.0.113.1/10.0.0.5"}, localIPs, false)
		require.NoError(t, err)
		require.Len(t, rules, 3)
		require.Equal(t, "10.0.0.5", rules[0].Local)
		require.Equal(t, []string{"203.0.113.1", "198.51.100.7"}, rules[0].External)
		v4, _ := catchAlls(t, rules)
		require.Equal(t, []string{"198.51.100.7"}, v4.External)
	})

	t.Run("other-family bare non-local is not attached to a local-keyed rule", func(t *testing.T) {
		rules, err := buildAdvertisedIPRules([]string{"2001:db8::1", "10.0.0.5"}, localIPs, false)
		require.NoError(t, err)
		require.Len(t, rules, 3) // identity + v4 drop + v6 catch-all
		require.Equal(t, "10.0.0.5", rules[0].Local)
		require.Equal(t, []string{"10.0.0.5"}, rules[0].External)
		v4, v6 := catchAlls(t, rules)
		require.Equal(t, []string{dropSentinelForIPv4Rule}, v4.External)
		require.Equal(t, []string{"2001:db8::1"}, v6.External)
	})

	t.Run("duplicate entries are deduplicated", func(t *testing.T) {
		rules, err := buildAdvertisedIPRules([]string{"203.0.113.1", "203.0.113.1", "10.0.0.5", "10.0.0.5"}, localIPs, false)
		require.NoError(t, err)
		require.Len(t, rules, 3)
		v4, _ := catchAlls(t, rules)
		require.Equal(t, []string{"203.0.113.1"}, v4.External)
	})

	t.Run("invalid IP errors", func(t *testing.T) {
		_, err := buildAdvertisedIPRules([]string{"not-an-ip"}, localIPs, false)
		require.Error(t, err)
	})

	t.Run("invalid pair errors", func(t *testing.T) {
		_, err := buildAdvertisedIPRules([]string{"203.0.113.1/nope"}, localIPs, false)
		require.Error(t, err)
		_, err = buildAdvertisedIPRules([]string{"a/b/c"}, localIPs, false)
		require.Error(t, err)
	})

	t.Run("dual-stack list gets a real catch-all per family", func(t *testing.T) {
		rules, err := buildAdvertisedIPRules([]string{"203.0.113.1", "2001:db8::1"}, localIPs, false)
		require.NoError(t, err)
		require.Len(t, rules, 2)
		v4, v6 := catchAlls(t, rules)
		require.Equal(t, []string{"203.0.113.1"}, v4.External)
		require.Equal(t, []string{"2001:db8::1"}, v6.External)
	})

	t.Run("v6-only list drops v4 with the sentinel", func(t *testing.T) {
		rules, err := buildAdvertisedIPRules([]string{"2001:db8::1"}, localIPs, false)
		require.NoError(t, err)
		v4, v6 := catchAlls(t, rules)
		require.Equal(t, []string{dropSentinelForIPv4Rule}, v4.External)
		require.Equal(t, []string{"2001:db8::1"}, v6.External)
	})

	t.Run("IPv6 spellings are canonicalized before local matching", func(t *testing.T) {
		v6Locals := []string{"2001:db8::1", "fd00::5"}
		// the long form of a LOCAL address must be recognised as local (identity
		// rule), not treated as NAT-side and attached to fd00::5's socket
		rules, err := buildAdvertisedIPRules([]string{"2001:0db8:0:0:0:0:0:1"}, v6Locals, false)
		require.NoError(t, err)
		require.Len(t, rules, 3) // identity + v4 drop + v6 drop
		require.Equal(t, "2001:db8::1", rules[0].Local)
		require.Equal(t, []string{"2001:db8::1"}, rules[0].External)
		_, v6 := catchAlls(t, rules)
		require.Equal(t, []string{dropSentinelForIPv6Rule}, v6.External)

		// pair components and duplicates in different spellings collapse too
		rules, err = buildAdvertisedIPRules([]string{
			"2001:0db8::10/2001:0db8:0:0:0:0:0:1",
			"2001:db8::10/2001:db8::1",
			"2001:DB8::1",
		}, v6Locals, false)
		require.NoError(t, err)
		require.Len(t, rules, 3)
		require.Equal(t, "2001:db8::1", rules[0].Local)
		require.ElementsMatch(t, []string{"2001:db8::10", "2001:db8::1"}, rules[0].External)
	})

	t.Run("validateAdvertisedIPs runs without network state", func(t *testing.T) {
		require.NoError(t, validateAdvertisedIPs(nil))
		require.NoError(t, validateAdvertisedIPs([]string{"203.0.113.1", "203.0.113.2/10.0.0.5", "2001:db8::1"}))
		require.Error(t, validateAdvertisedIPs([]string{"203.0.113.1", "bad"}))
		require.Error(t, validateAdvertisedIPs([]string{"203.0.113.1/nope"}))
		require.Error(t, validateAdvertisedIPs([]string{"a/b/c"}))
	})

	t.Run("shared-port gathering: bare locals are advertised for every socket", func(t *testing.T) {
		// The production shape this exists for: a public IP that is only a
		// loopback alias (so it is "local" but has no gathering socket of its own)
		// plus the LAN IP the SFU is reached on from inside the NAT. Both must be
		// advertised from the one socket that actually gathers (the bridge IP).
		rules, err := buildAdvertisedIPRules(
			[]string{"129.207.5.91", "10.1.160.20"},
			[]string{"129.207.5.91", "172.18.0.4"}, true)
		require.NoError(t, err)
		require.Len(t, rules, 2) // no identity rules: v4 catch-all + v6 drop
		v4, v6 := catchAlls(t, rules)
		require.Equal(t, []string{"129.207.5.91", "10.1.160.20"}, v4.External)
		require.Equal(t, []string{dropSentinelForIPv6Rule}, v6.External)

		// pairs stay socket-scoped even in shared-port mode, but gain the
		// everywhere set
		rules, err = buildAdvertisedIPRules([]string{"203.0.113.1/10.0.0.5", "198.51.100.7", "192.168.1.10"}, localIPs, true)
		require.NoError(t, err)
		require.Len(t, rules, 3) // pair + v4 catch-all + v6 drop
		require.Equal(t, "10.0.0.5", rules[0].Local)
		require.Equal(t, []string{"203.0.113.1", "198.51.100.7", "192.168.1.10"}, rules[0].External)
		v4, _ = catchAlls(t, rules)
		require.Equal(t, []string{"198.51.100.7", "192.168.1.10"}, v4.External)

		// same input in port-range mode keeps the local socket-scoped
		rules, err = buildAdvertisedIPRules([]string{"129.207.5.91", "10.1.160.20"}, []string{"129.207.5.91", "172.18.0.4"}, false)
		require.NoError(t, err)
		require.Len(t, rules, 3) // identity + v4 catch-all + v6 drop
		require.Equal(t, "129.207.5.91", rules[0].Local)
		v4, _ = catchAlls(t, rules)
		require.Equal(t, []string{"10.1.160.20"}, v4.External)
	})

	t.Run("rules are accepted by the pion setting engine", func(t *testing.T) {
		for _, entries := range [][]string{
			{"203.0.113.1", "203.0.113.2"},
			{"203.0.113.1", "10.0.0.5"},
			{"10.0.0.5", "192.168.1.10"},
			{"203.0.113.1/10.0.0.5"},
			{"2001:db8::1"},
			{"203.0.113.1", "2001:db8::1"},
		} {
			rules, err := buildAdvertisedIPRules(entries, localIPs, false)
			require.NoError(t, err)
			var se webrtc.SettingEngine
			require.NoError(t, se.SetICEAddressRewriteRules(rules...), "entries %v", entries)
		}
	})

	t.Run("rules pass pion agent validation end to end", func(t *testing.T) {
		// SetICEAddressRewriteRules only stores the rules; pion sanitizes them when
		// the ICE agent is built (which is where an empty External list used to fail
		// with "invalid address rewrite mapping"). Build a real agent for every shape,
		// including the drop-only shapes that rely on the sentinels.
		for _, entries := range [][]string{
			{"203.0.113.1", "203.0.113.2"},
			{"203.0.113.1", "10.0.0.5"},
			{"10.0.0.5", "192.168.1.10"},
			{"203.0.113.1/10.0.0.5"},
			{"2001:db8::1"},
			{"203.0.113.1", "2001:db8::1"},
		} {
			rules, err := buildAdvertisedIPRules(entries, localIPs, false)
			require.NoError(t, err)
			iceRules := make([]ice.AddressRewriteRule, 0, len(rules))
			for _, r := range rules {
				iceRules = append(iceRules, ice.AddressRewriteRule{
					External:        r.External,
					Local:           r.Local,
					AsCandidateType: ice.CandidateType(r.AsCandidateType),
					Mode:            ice.AddressRewriteMode(r.Mode),
					Networks:        toICENetworkTypes(r.Networks),
				})
			}
			agent, err := ice.NewAgentWithOptions(ice.WithAddressRewriteRules(iceRules...))
			require.NoError(t, err, "entries %v", entries)
			require.NoError(t, agent.Close())
		}
	})
}

func toICENetworkTypes(nts []webrtc.NetworkType) []ice.NetworkType {
	out := make([]ice.NetworkType, 0, len(nts))
	for _, nt := range nts {
		out = append(out, ice.NetworkType(nt))
	}
	return out
}

// gatherHostAddresses builds a real ICE agent with the rules for `entries`
// against this machine's actual interfaces, gathers IPv4 host candidates, and
// returns the distinct advertised addresses.
func gatherHostAddresses(t *testing.T, entries, localIPs []string, sharedPort bool) []string {
	t.Helper()
	rules, err := buildAdvertisedIPRules(entries, localIPs, sharedPort)
	require.NoError(t, err)
	iceRules := make([]ice.AddressRewriteRule, 0, len(rules))
	for _, r := range rules {
		iceRules = append(iceRules, ice.AddressRewriteRule{
			External:        r.External,
			Local:           r.Local,
			AsCandidateType: ice.CandidateType(r.AsCandidateType),
			Mode:            ice.AddressRewriteMode(r.Mode),
			Networks:        toICENetworkTypes(r.Networks),
		})
	}
	agent, err := ice.NewAgentWithOptions(
		ice.WithNetworkTypes([]ice.NetworkType{ice.NetworkTypeUDP4}),
		ice.WithCandidateTypes([]ice.CandidateType{ice.CandidateTypeHost}),
		ice.WithAddressRewriteRules(iceRules...),
	)
	require.NoError(t, err)
	defer agent.Close()

	var (
		mu   sync.Mutex
		seen []string
		done = make(chan struct{})
	)
	require.NoError(t, agent.OnCandidate(func(c ice.Candidate) {
		if c == nil {
			close(done)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if !slices.Contains(seen, c.Address()) {
			seen = append(seen, c.Address())
		}
	}))
	require.NoError(t, agent.GatherCandidates())
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("candidate gathering did not complete")
	}
	mu.Lock()
	defer mu.Unlock()
	return slices.Clone(seen)
}

// TestAdvertisedIPRulesGatherExactlyTheList is the end-to-end contract: what a
// real pion agent advertises after gathering is exactly the configured list —
// in particular the sentinel drop rules really do suppress unlisted locals on
// both gather paths, which the setting-engine and agent-construction tests
// above cannot show.
func TestAdvertisedIPRulesGatherExactlyTheList(t *testing.T) {
	localIPs, err := rtcconfig.GetLocalIPAddresses(false, true, nil, nil)
	require.NoError(t, err)
	var localV4 []string
	for _, ip := range localIPs {
		if net.ParseIP(ip).To4() != nil {
			localV4 = append(localV4, ip)
		}
	}
	if len(localV4) == 0 {
		t.Skip("no non-loopback IPv4 interface available")
	}
	chosen := localV4[0]

	t.Run("bare non-local replaces every local", func(t *testing.T) {
		got := gatherHostAddresses(t, []string{"203.0.113.1"}, localIPs, false)
		require.Equal(t, []string{"203.0.113.1"}, got)
	})

	t.Run("bare local advertises itself and drops the other locals", func(t *testing.T) {
		got := gatherHostAddresses(t, []string{chosen}, localIPs, false)
		require.Equal(t, []string{chosen}, got, "unlisted locals %v must be dropped", localV4)
	})

	t.Run("pair advertises its external for that socket only", func(t *testing.T) {
		got := gatherHostAddresses(t, []string{"203.0.113.1/" + chosen}, localIPs, false)
		require.Equal(t, []string{"203.0.113.1"}, got)
	})

	t.Run("local plus non-local advertises both and nothing else", func(t *testing.T) {
		got := gatherHostAddresses(t, []string{"203.0.113.1", chosen}, localIPs, false)
		require.ElementsMatch(t, []string{"203.0.113.1", chosen}, got)
	})
}

func TestApplyAdvertisedIPsClearsDiscoveryState(t *testing.T) {
	// Simulate what rtcconfig.NewWebRTCConfig leaves behind when use_external_ip
	// discovered a mapping (NAT1To1IPs, consumed by the Firefox path in
	// newPeerConnection) and when the node IP was auto-generated (automatic STUN
	// servers). Both must be cleared once the explicit list is authoritative.
	webRTCConfig := &rtcconfig.WebRTCConfig{
		Configuration: webrtc.Configuration{
			ICEServers: []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}},
		},
		NAT1To1IPs: []string{"198.51.100.20/10.0.0.5"},
	}
	rtcConf := &config.RTCConfig{}
	rtcConf.AdvertisedIPs = []string{"203.0.113.10"}

	require.NoError(t, applyAdvertisedIPs(webRTCConfig, rtcConf))
	require.Nil(t, webRTCConfig.NAT1To1IPs, "legacy NAT1To1IPs must not override the explicit list for Firefox")
	require.Nil(t, webRTCConfig.Configuration.ICEServers, "automatic STUN must not add discovery-derived srflx candidates")
}

func TestUsesSharedPortGathering(t *testing.T) {
	var c config.RTCConfig
	require.True(t, usesSharedPortGathering(&c), "udp_port / mux is the default")
	c.ICEPortRangeStart, c.ICEPortRangeEnd = 50000, 60000
	require.False(t, usesSharedPortGathering(&c))
	c.ICEPortRangeEnd = 0
	require.True(t, usesSharedPortGathering(&c), "half-configured range is ignored by NewWebRTCConfig too")
}

// TestAdvertisedIPRulesGatherViaUDPMux exercises the UDP-mux gather path
// (gatherCandidatesLocalUDPMux), which is what a production server uses: one
// socket, every local interface address reported against the same port. It
// asserts the shared-port semantics on that path — listed local addresses and
// NAT-side addresses are all advertised, unlisted locals are not.
func TestAdvertisedIPRulesGatherViaUDPMux(t *testing.T) {
	localIPs, err := rtcconfig.GetLocalIPAddresses(false, true, nil, nil)
	require.NoError(t, err)
	var localV4 []string
	for _, ip := range localIPs {
		if net.ParseIP(ip).To4() != nil {
			localV4 = append(localV4, ip)
		}
	}
	if len(localV4) == 0 {
		t.Skip("no non-loopback IPv4 interface available")
	}
	chosen := localV4[0]

	gather := func(entries []string) []string {
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
		require.NoError(t, err)
		mux := ice.NewUDPMuxDefault(ice.UDPMuxParams{UDPConn: conn})
		defer mux.Close()

		rules, err := buildAdvertisedIPRules(entries, localIPs, true)
		require.NoError(t, err)
		iceRules := make([]ice.AddressRewriteRule, 0, len(rules))
		for _, r := range rules {
			iceRules = append(iceRules, ice.AddressRewriteRule{
				External:        r.External,
				Local:           r.Local,
				AsCandidateType: ice.CandidateType(r.AsCandidateType),
				Mode:            ice.AddressRewriteMode(r.Mode),
				Networks:        toICENetworkTypes(r.Networks),
			})
		}
		agent, err := ice.NewAgentWithOptions(
			ice.WithNetworkTypes([]ice.NetworkType{ice.NetworkTypeUDP4}),
			ice.WithCandidateTypes([]ice.CandidateType{ice.CandidateTypeHost}),
			ice.WithUDPMux(mux),
			ice.WithAddressRewriteRules(iceRules...),
		)
		require.NoError(t, err)
		defer agent.Close()

		var (
			mu   sync.Mutex
			seen []string
			done = make(chan struct{})
		)
		require.NoError(t, agent.OnCandidate(func(c ice.Candidate) {
			if c == nil {
				close(done)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if !slices.Contains(seen, c.Address()) {
				seen = append(seen, c.Address())
			}
		}))
		require.NoError(t, agent.GatherCandidates())
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("candidate gathering did not complete")
		}
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(seen)
	}

	t.Run("bare local plus NAT-side: both advertised, nothing else", func(t *testing.T) {
		require.ElementsMatch(t, []string{chosen, "203.0.113.1"}, gather([]string{chosen, "203.0.113.1"}))
	})
	t.Run("bare local only: itself, unlisted locals dropped", func(t *testing.T) {
		require.Equal(t, []string{chosen}, gather([]string{chosen}))
	})
	t.Run("NAT-side only", func(t *testing.T) {
		require.Equal(t, []string{"203.0.113.1"}, gather([]string{"203.0.113.1"}))
	})
}

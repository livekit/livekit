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
	"testing"

	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"
)

func TestBuildAdvertisedIPRules(t *testing.T) {
	localIPs := []string{"10.0.0.5", "192.168.1.10"}

	t.Run("pure external list produces a single replace catch-all", func(t *testing.T) {
		rules, err := buildAdvertisedIPRules([]string{"203.0.113.1", "203.0.113.2"}, localIPs)
		require.NoError(t, err)
		require.Len(t, rules, 2) // catch-all + IPv6 drop rule
		require.Equal(t, []string{"203.0.113.1", "203.0.113.2"}, rules[0].External)
		require.Empty(t, rules[0].Local)
		require.Equal(t, webrtc.ICECandidateTypeHost, rules[0].AsCandidateType)
		require.Equal(t, webrtc.ICEAddressRewriteReplace, rules[0].Mode)
	})

	t.Run("local and external mix advertises both, suppresses unlisted locals", func(t *testing.T) {
		rules, err := buildAdvertisedIPRules([]string{"203.0.113.1", "10.0.0.5"}, localIPs)
		require.NoError(t, err)
		require.Len(t, rules, 3) // identity + catch-all + IPv6 drop rule

		// identity rule shields the listed local from the catch-all
		require.Equal(t, []string{"10.0.0.5"}, rules[0].External)
		require.Equal(t, "10.0.0.5", rules[0].Local)
		require.Equal(t, webrtc.ICEAddressRewriteReplace, rules[0].Mode)

		// catch-all rewrites every other local (e.g. 192.168.1.10) to the advertised set
		require.Equal(t, []string{"203.0.113.1", "10.0.0.5"}, rules[1].External)
		require.Empty(t, rules[1].Local)
		require.Equal(t, webrtc.ICEAddressRewriteReplace, rules[1].Mode)
	})

	t.Run("two local IPs advertise themselves only", func(t *testing.T) {
		rules, err := buildAdvertisedIPRules([]string{"10.0.0.5", "192.168.1.10"}, localIPs)
		require.NoError(t, err)
		require.Len(t, rules, 4) // 2 identity + catch-all + IPv6 drop rule
		require.Equal(t, "10.0.0.5", rules[0].Local)
		require.Equal(t, []string{"10.0.0.5"}, rules[0].External)
		require.Equal(t, "192.168.1.10", rules[1].Local)
		require.Equal(t, []string{"192.168.1.10"}, rules[1].External)
		// catch-all still present for any other local interface
		require.Empty(t, rules[2].Local)
		require.Equal(t, []string{"10.0.0.5", "192.168.1.10"}, rules[2].External)
	})

	t.Run("external/local pair replaces the local", func(t *testing.T) {
		rules, err := buildAdvertisedIPRules([]string{"203.0.113.1/10.0.0.5"}, localIPs)
		require.NoError(t, err)
		require.Len(t, rules, 3) // pair + catch-all + IPv6 drop rule
		require.Equal(t, []string{"203.0.113.1"}, rules[0].External)
		require.Equal(t, "10.0.0.5", rules[0].Local)
		require.Equal(t, webrtc.ICEAddressRewriteReplace, rules[0].Mode)
	})

	t.Run("pair local also listed bare advertises both", func(t *testing.T) {
		rules, err := buildAdvertisedIPRules([]string{"203.0.113.1/10.0.0.5", "10.0.0.5"}, localIPs)
		require.NoError(t, err)
		require.Len(t, rules, 3)
		require.Equal(t, []string{"203.0.113.1", "10.0.0.5"}, rules[0].External)
		require.Equal(t, "10.0.0.5", rules[0].Local)
	})

	t.Run("multiple pairs for the same local merge into one rule", func(t *testing.T) {
		rules, err := buildAdvertisedIPRules([]string{"203.0.113.1/10.0.0.5", "203.0.113.2/10.0.0.5"}, localIPs)
		require.NoError(t, err)
		require.Len(t, rules, 3)
		require.Equal(t, []string{"203.0.113.1", "203.0.113.2"}, rules[0].External)
		require.Equal(t, "10.0.0.5", rules[0].Local)
	})

	t.Run("duplicate entries are deduplicated", func(t *testing.T) {
		rules, err := buildAdvertisedIPRules([]string{"203.0.113.1", "203.0.113.1"}, localIPs)
		require.NoError(t, err)
		require.Len(t, rules, 2)
		require.Equal(t, []string{"203.0.113.1"}, rules[0].External)
	})

	t.Run("invalid IP errors", func(t *testing.T) {
		_, err := buildAdvertisedIPRules([]string{"not-an-ip"}, localIPs)
		require.Error(t, err)
	})

	t.Run("invalid pair errors", func(t *testing.T) {
		_, err := buildAdvertisedIPRules([]string{"203.0.113.1/nope"}, localIPs)
		require.Error(t, err)
		_, err = buildAdvertisedIPRules([]string{"1.2.3.4/5.6.7.8/9.10.11.12"}, localIPs)
		require.Error(t, err)
	})

	t.Run("v4-only list appends an IPv6 drop rule; dual-stack list appends none", func(t *testing.T) {
		rules, err := buildAdvertisedIPRules([]string{"203.0.113.1"}, localIPs)
		require.NoError(t, err)
		last := rules[len(rules)-1]
		require.Empty(t, last.External)
		require.Equal(t, webrtc.ICEAddressRewriteReplace, last.Mode)
		require.Equal(t, []webrtc.NetworkType{webrtc.NetworkTypeUDP6, webrtc.NetworkTypeTCP6}, last.Networks)

		rules, err = buildAdvertisedIPRules([]string{"203.0.113.1", "2001:db8::1"}, localIPs)
		require.NoError(t, err)
		for _, r := range rules {
			require.NotEmpty(t, r.External)
		}
	})

	t.Run("rules are accepted by the pion setting engine", func(t *testing.T) {
		rules, err := buildAdvertisedIPRules(
			[]string{"203.0.113.1", "10.0.0.5", "198.51.100.7/192.168.1.10"}, localIPs)
		require.NoError(t, err)
		se := webrtc.SettingEngine{}
		require.NoError(t, se.SetICEAddressRewriteRules(rules...))
	})
}

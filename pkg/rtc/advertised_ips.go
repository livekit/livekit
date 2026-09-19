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
	"fmt"
	"net"
	"slices"
	"strings"

	"github.com/pion/webrtc/v4"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/mediatransportutil/pkg/rtcconfig"
	"github.com/livekit/protocol/logger"
)

// buildAdvertisedIPRules translates rtc.advertised_ips into pion address-rewrite
// rules implementing "advertise exactly this list of addresses":
//
//   - a bare entry that matches a local IP gets an identity rule, so that local
//     address is advertised as itself;
//   - an "external/local" pair maps the local socket to the external address
//     (both are advertised if the local is also listed bare);
//   - every other local address is caught by a catch-all rule that rewrites it
//     to the advertised set, so nothing outside the list ever reaches the SDP.
//
// All rules use replace mode: each rule's External list IS the complete set of
// addresses to advertise for the local addresses it matches. Explicit
// Local-keyed rules take precedence over the catch-all in pion's evaluation.
func buildAdvertisedIPRules(entries []string, localIPs []string) ([]webrtc.ICEAddressRewriteRule, error) {
	var bareIPs []string
	pairExternals := map[string][]string{} // local -> externals from pair entries
	var pairLocals []string                // iteration order for determinism

	for _, entry := range entries {
		parts := strings.Split(strings.TrimSpace(entry), "/")
		switch len(parts) {
		case 1:
			ip := parts[0]
			if net.ParseIP(ip) == nil {
				return nil, fmt.Errorf("rtc.advertised_ips: invalid IP %q", entry)
			}
			if !slices.Contains(bareIPs, ip) {
				bareIPs = append(bareIPs, ip)
			}
		case 2:
			external, local := parts[0], parts[1]
			if net.ParseIP(external) == nil || net.ParseIP(local) == nil {
				return nil, fmt.Errorf("rtc.advertised_ips: invalid external/local pair %q", entry)
			}
			if !slices.Contains(pairExternals[local], external) {
				pairExternals[local] = append(pairExternals[local], external)
			}
			if !slices.Contains(pairLocals, local) {
				pairLocals = append(pairLocals, local)
			}
		default:
			return nil, fmt.Errorf("rtc.advertised_ips: invalid entry %q, expected \"ip\" or \"external/local\"", entry)
		}
	}

	// advertised is the complete set of addresses that may appear in the SDP
	var advertised []string
	appendUnique := func(ips ...string) {
		for _, ip := range ips {
			if !slices.Contains(advertised, ip) {
				advertised = append(advertised, ip)
			}
		}
	}
	appendUnique(bareIPs...)
	for _, local := range pairLocals {
		appendUnique(pairExternals[local]...)
	}

	var rules []webrtc.ICEAddressRewriteRule

	// explicit rule per local address that appears in the list
	handledLocals := make([]string, 0, len(bareIPs)+len(pairLocals))
	for _, local := range pairLocals {
		externals := slices.Clone(pairExternals[local])
		if slices.Contains(bareIPs, local) {
			// listed bare as well: advertise the local itself too
			externals = append(externals, local)
		}
		rules = append(rules, webrtc.ICEAddressRewriteRule{
			External:        externals,
			Local:           local,
			AsCandidateType: webrtc.ICECandidateTypeHost,
			Mode:            webrtc.ICEAddressRewriteReplace,
		})
		handledLocals = append(handledLocals, local)
	}
	for _, ip := range bareIPs {
		if !slices.Contains(localIPs, ip) || slices.Contains(handledLocals, ip) {
			continue
		}
		// identity rule: advertise this local address as itself,
		// shielding it from the catch-all below
		rules = append(rules, webrtc.ICEAddressRewriteRule{
			External:        []string{ip},
			Local:           ip,
			AsCandidateType: webrtc.ICECandidateTypeHost,
			Mode:            webrtc.ICEAddressRewriteReplace,
		})
		handledLocals = append(handledLocals, ip)
	}

	// catch-all: any other local address advertises the listed set instead of
	// itself (duplicates across sockets are deduplicated by pion)
	rules = append(rules, webrtc.ICEAddressRewriteRule{
		External:        advertised,
		AsCandidateType: webrtc.ICECandidateTypeHost,
		Mode:            webrtc.ICEAddressRewriteReplace,
	})

	// pion partitions rewrite mappings by address family, so a family with no
	// advertised addresses would leave that family's local candidates unmatched
	// and therefore advertised as-is. Add an empty replace-mode drop rule per
	// absent family so the list stays authoritative on dual-stack hosts.
	hasV4, hasV6 := false, false
	for _, ip := range advertised {
		if net.ParseIP(ip).To4() != nil {
			hasV4 = true
		} else {
			hasV6 = true
		}
	}
	if !hasV4 {
		rules = append(rules, webrtc.ICEAddressRewriteRule{
			AsCandidateType: webrtc.ICECandidateTypeHost,
			Mode:            webrtc.ICEAddressRewriteReplace,
			Networks:        []webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeTCP4},
		})
	}
	if !hasV6 {
		rules = append(rules, webrtc.ICEAddressRewriteRule{
			AsCandidateType: webrtc.ICECandidateTypeHost,
			Mode:            webrtc.ICEAddressRewriteReplace,
			Networks:        []webrtc.NetworkType{webrtc.NetworkTypeUDP6, webrtc.NetworkTypeTCP6},
		})
	}

	return rules, nil
}

// applyAdvertisedIPs installs the explicit candidate-advertisement list on the
// setting engine, overriding whatever rewrite rules automatic discovery
// (node_ip / use_external_ip) configured.
func applyAdvertisedIPs(se *webrtc.SettingEngine, rtcConf *config.RTCConfig) error {
	var ifFilter func(string) bool
	if len(rtcConf.Interfaces.Includes) != 0 || len(rtcConf.Interfaces.Excludes) != 0 {
		ifFilter = rtcconfig.InterfaceFilterFromConf(rtcConf.Interfaces)
	}
	var ipFilter func(net.IP) bool
	if len(rtcConf.IPs.Includes) != 0 || len(rtcConf.IPs.Excludes) != 0 {
		filter, err := rtcconfig.IPFilterFromConf(rtcConf.IPs)
		if err != nil {
			return err
		}
		ipFilter = filter
	}

	localIPs, err := rtcconfig.GetLocalIPAddresses(rtcConf.EnableLoopbackCandidate, true, ifFilter, ipFilter)
	if err != nil {
		return fmt.Errorf("rtc.advertised_ips: could not enumerate local IPs: %w", err)
	}

	rules, err := buildAdvertisedIPRules(rtcConf.AdvertisedIPs, localIPs)
	if err != nil {
		return err
	}

	if rtcConf.UseExternalIP {
		logger.Warnw("rtc.advertised_ips overrides use_external_ip-derived candidate advertisement", nil)
	}

	// deliberately loud: misconfigured advertisement is otherwise invisible
	// until clients fail ICE, so log exactly what will be advertised
	logger.Infow("using explicit advertised IPs for ICE candidates",
		"entries", rtcConf.AdvertisedIPs,
		"localIPs", localIPs,
	)

	if err := se.SetICEAddressRewriteRules(rules...); err != nil {
		return fmt.Errorf("rtc.advertised_ips: %w", err)
	}

	return nil
}

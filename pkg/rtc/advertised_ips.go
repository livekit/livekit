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

// Sentinel externals for drop rules. pion rejects a rewrite rule whose External
// list is empty, but its mapper partitions externals by address family and
// discards any external whose family a rule's Networks filter excludes. A rule
// scoped to IPv4 networks that carries only an IPv6 external therefore ends up
// as a valid catch-all with no usable externals — which in replace mode drops
// every candidate it matches. These addresses are never advertised.
const (
	dropSentinelForIPv4Rule = "::1"     // IPv6, stripped from an IPv4-scoped rule
	dropSentinelForIPv6Rule = "0.0.0.0" // IPv4, stripped from an IPv6-scoped rule
)

var (
	ipv4Networks = []webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeTCP4}
	ipv6Networks = []webrtc.NetworkType{webrtc.NetworkTypeUDP6, webrtc.NetworkTypeTCP6}
)

// buildAdvertisedIPRules translates rtc.advertised_ips into pion address-rewrite
// rules implementing "advertise exactly this list of addresses":
//
//   - a bare entry that is NOT a local IP is a NAT-side address that reaches this
//     host whichever socket answers: it is advertised for every socket of its
//     family (explicit rules and catch-all alike);
//   - a bare entry that matches a local IP gets an identity rule, so that local
//     socket is advertised as itself (plus the NAT-side addresses);
//   - an "external/local" pair maps the local socket to the external address
//     (both are advertised if the local is also listed bare). Pair externals and
//     bare local entries are interface-scoped and deliberately kept OUT of the
//     catch-all: pion preserves the original socket's port when rewriting, and
//     with port-range gathering a different interface's socket has a different
//     port, so advertising an interface-scoped address for it would produce a
//     candidate with no listener behind it;
//   - every other local socket is caught by a per-family catch-all advertising
//     the NAT-side addresses, or by a drop rule when there are none of that
//     family, so unlisted locals never reach the SDP (dual-stack hosts included).
//
// All rules use replace mode: each rule's External list IS the complete set of
// addresses to advertise for the local sockets it matches. Explicit Local-keyed
// rules take precedence over catch-alls in pion's evaluation.
func buildAdvertisedIPRules(entries []string, localIPs []string) ([]webrtc.ICEAddressRewriteRule, error) {
	bareIPs, pairExternals, pairLocals, err := parseAdvertisedIPs(entries)
	if err != nil {
		return nil, err
	}

	// Bare entries that are NOT local addresses are the NAT-side addresses that
	// reach this host regardless of which socket answers, so they are advertised
	// for EVERY socket: appended to each explicit rule below and forming the
	// per-family catch-all for unlisted sockets. (Under a UDP mux every socket
	// shares one port, so pion's (address, port) dedup collapses them to one
	// candidate; under port-range gathering an operator who needs a NAT-side
	// address tied to one socket should use the "external/local" pair form.)
	// Split by family: an explicit Local-keyed rule takes the local's family, so
	// an other-family external there would yield a mismatched-family candidate.
	var bareNonLocalV4, bareNonLocalV6 []string
	for _, ip := range bareIPs {
		if slices.Contains(localIPs, ip) {
			continue
		}
		if net.ParseIP(ip).To4() != nil {
			bareNonLocalV4 = append(bareNonLocalV4, ip)
		} else {
			bareNonLocalV6 = append(bareNonLocalV6, ip)
		}
	}
	bareNonLocalFor := func(local string) []string {
		if net.ParseIP(local).To4() != nil {
			return bareNonLocalV4
		}
		return bareNonLocalV6
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
		externals = appendUnique(externals, bareNonLocalFor(local)...)
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
		// identity rule: advertise this local address as itself (plus the
		// NAT-side addresses), shielding it from the catch-all below
		rules = append(rules, webrtc.ICEAddressRewriteRule{
			External:        appendUnique([]string{ip}, bareNonLocalFor(ip)...),
			Local:           ip,
			AsCandidateType: webrtc.ICECandidateTypeHost,
			Mode:            webrtc.ICEAddressRewriteReplace,
		})
		handledLocals = append(handledLocals, ip)
	}

	// per-family catch-all for unlisted local sockets, or a drop rule when
	// there is nothing NAT-side of that family to advertise for them
	rules = append(rules, catchAllOrDropRule(bareNonLocalV4, ipv4Networks, dropSentinelForIPv4Rule))
	rules = append(rules, catchAllOrDropRule(bareNonLocalV6, ipv6Networks, dropSentinelForIPv6Rule))

	return rules, nil
}

// parseAdvertisedIPs validates rtc.advertised_ips and splits it into bare entries
// and external/local pairs. Every address is canonicalized through
// net.IP.String() so that the textual form an operator used (e.g.
// "2001:0db8:0:0:0:0:0:1") compares equal to the normalized form the interface
// enumeration returns ("2001:db8::1") — otherwise a local address spelled
// differently would be misclassified as NAT-side and attached to every socket.
// It has no side effects and needs no network state, so callers can run it
// before opening any listener to reject bad config early.
func parseAdvertisedIPs(entries []string) (bareIPs []string, pairExternals map[string][]string, pairLocals []string, err error) {
	pairExternals = map[string][]string{} // local -> externals from pair entries; pairLocals keeps iteration order

	canonical := func(s string) (string, bool) {
		ip := net.ParseIP(strings.TrimSpace(s))
		if ip == nil {
			return "", false
		}
		return ip.String(), true
	}

	for _, entry := range entries {
		parts := strings.Split(strings.TrimSpace(entry), "/")
		switch len(parts) {
		case 1:
			ip, ok := canonical(parts[0])
			if !ok {
				return nil, nil, nil, fmt.Errorf("rtc.advertised_ips: invalid IP %q", entry)
			}
			if !slices.Contains(bareIPs, ip) {
				bareIPs = append(bareIPs, ip)
			}
		case 2:
			external, okE := canonical(parts[0])
			local, okL := canonical(parts[1])
			if !okE || !okL {
				return nil, nil, nil, fmt.Errorf("rtc.advertised_ips: invalid external/local pair %q", entry)
			}
			if !slices.Contains(pairExternals[local], external) {
				pairExternals[local] = append(pairExternals[local], external)
			}
			if !slices.Contains(pairLocals, local) {
				pairLocals = append(pairLocals, local)
			}
		default:
			return nil, nil, nil, fmt.Errorf("rtc.advertised_ips: invalid entry %q, expected \"ip\" or \"external/local\"", entry)
		}
	}
	return bareIPs, pairExternals, pairLocals, nil
}

// validateAdvertisedIPs reports whether rtc.advertised_ips is well-formed. It is
// meant to run BEFORE rtcconfig.NewWebRTCConfig opens the UDP mux and TCP
// listener, so a config error never leaves bound sockets behind.
func validateAdvertisedIPs(entries []string) error {
	_, _, _, err := parseAdvertisedIPs(entries)
	return err
}

// closeRTCListeners releases the sockets rtcconfig.NewWebRTCConfig bound (UDP
// mux, TCP mux listener) when configuration fails after construction, so a
// caller that fixes its config and retries does not hit address-in-use.
func closeRTCListeners(c *rtcconfig.WebRTCConfig) {
	if c == nil {
		return
	}
	if c.UDPMux != nil {
		if err := c.UDPMux.Close(); err != nil {
			logger.Warnw("could not close UDP mux after config error", err)
		}
	}
	if c.TCPMuxListener != nil {
		if err := c.TCPMuxListener.Close(); err != nil {
			logger.Warnw("could not close TCP mux listener after config error", err)
		}
	}
}

func appendUnique(dst []string, more ...string) []string {
	for _, s := range more {
		if !slices.Contains(dst, s) {
			dst = append(dst, s)
		}
	}
	return dst
}

// catchAllOrDropRule returns the family-scoped rule for unlisted local sockets:
// a replace-mode catch-all advertising `externals`, or — when there is nothing
// of that family to advertise — a drop rule carrying only the other-family
// sentinel, which pion's family filter strips (see the sentinel constants).
func catchAllOrDropRule(externals []string, networks []webrtc.NetworkType, dropSentinel string) webrtc.ICEAddressRewriteRule {
	if len(externals) == 0 {
		externals = []string{dropSentinel}
	}
	return webrtc.ICEAddressRewriteRule{
		External:        externals,
		AsCandidateType: webrtc.ICECandidateTypeHost,
		Mode:            webrtc.ICEAddressRewriteReplace,
		Networks:        networks,
	}
}

// advertisedAddressSet returns every address that may appear in the SDP for the
// given entries (bare IPs plus pair externals), for logging.
func advertisedAddressSet(entries []string) []string {
	var out []string
	for _, entry := range entries {
		ip := strings.Split(strings.TrimSpace(entry), "/")[0]
		if !slices.Contains(out, ip) {
			out = append(out, ip)
		}
	}
	return out
}

// applyAdvertisedIPs installs the explicit candidate-advertisement list on the
// WebRTC config, overriding whatever automatic discovery (node_ip /
// use_external_ip / STUN) configured for candidate advertisement:
//
//   - the setting engine's address-rewrite rules are replaced by the list;
//   - WebRTCConfig.NAT1To1IPs is cleared. It only feeds the per-peer-connection
//     legacy rewrite for clients without prflx-over-relay support (Firefox),
//     which would otherwise reinstall discovery-derived mappings on top of the
//     authoritative list for those clients;
//   - Configuration.ICEServers is cleared. It holds only the STUN servers that
//     NewWebRTCConfig adds when the node IP was auto-generated; their
//     server-reflexive candidates would advertise discovery-derived addresses
//     outside the list. (TURN is handed to clients in the join response, not
//     here, so it is unaffected.)
func applyAdvertisedIPs(webRTCConfig *rtcconfig.WebRTCConfig, rtcConf *config.RTCConfig) error {
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
	if len(webRTCConfig.Configuration.ICEServers) > 0 {
		logger.Infow("rtc.advertised_ips: disabling automatic STUN servers", "iceServers", webRTCConfig.Configuration.ICEServers)
	}

	// deliberately loud: misconfigured advertisement is otherwise invisible
	// until clients fail ICE, so log exactly what will be advertised
	logger.Infow("using explicit advertised IPs for ICE candidates",
		"entries", rtcConf.AdvertisedIPs,
		"advertised", advertisedAddressSet(rtcConf.AdvertisedIPs),
		"localIPs", localIPs,
	)

	if err := webRTCConfig.SettingEngine.SetICEAddressRewriteRules(rules...); err != nil {
		return fmt.Errorf("rtc.advertised_ips: %w", err)
	}
	webRTCConfig.NAT1To1IPs = nil
	webRTCConfig.Configuration.ICEServers = nil

	return nil
}

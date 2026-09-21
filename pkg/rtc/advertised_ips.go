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
//   - a bare entry that matches a local IP depends on how candidates are gathered
//     (sharedPort):
//   - UDP-mux / TCP-mux gathering (sharedPort=true, the production default):
//     every socket shares the same port, so any listed address reaches every
//     socket. Bare local entries are advertised for every socket exactly like
//     NAT-side ones. This also covers a local address that has no gathering
//     socket of its own — e.g. a public IP aliased onto `lo` so the host can
//     reach itself — which an identity rule bound to "its" socket would never
//     advertise;
//   - port-range gathering (sharedPort=false): each interface's socket has a
//     different port, and pion preserves the socket's port when rewriting, so
//     advertising an address for a socket it does not reach yields a candidate
//     with no listener behind it. A bare local entry therefore gets an identity
//     rule scoped to its own socket (plus the NAT-side addresses);
//   - an "external/local" pair maps the local socket to the external address
//     (both are advertised if the local is also listed bare); the external is
//     always socket-scoped and never enters the catch-all;
//   - every other local socket is caught by a per-family catch-all advertising
//     the everywhere-valid addresses, or by a drop rule when there are none of
//     that family, so unlisted locals never reach the SDP (dual-stack included).
//
// All rules use replace mode: each rule's External list IS the complete set of
// addresses to advertise for the local sockets it matches. Explicit Local-keyed
// rules take precedence over catch-alls in pion's evaluation.
func buildAdvertisedIPRules(entries []string, localIPs []string, sharedPort bool) ([]webrtc.ICEAddressRewriteRule, error) {
	bareIPs, pairExternals, pairLocals, err := parseAdvertisedIPs(entries)
	if err != nil {
		return nil, err
	}

	// The per-family "everywhere" set: bare entries valid for EVERY socket —
	// NAT-side (non-local) addresses always, and local ones too when all sockets
	// share a port. Appended to each explicit rule below and forming the catch-all
	// for unlisted sockets. Split by family: an explicit Local-keyed rule takes the
	// local's family, so an other-family external there would yield a
	// mismatched-family candidate. (pion dedups candidates by (address, port), so
	// the same address reached from several sockets collapses to one.)
	var everywhereV4, everywhereV6 []string
	for _, ip := range bareIPs {
		if !sharedPort && slices.Contains(localIPs, ip) {
			continue // socket-scoped; handled by its identity rule below
		}
		if net.ParseIP(ip).To4() != nil {
			everywhereV4 = append(everywhereV4, ip)
		} else {
			everywhereV6 = append(everywhereV6, ip)
		}
	}
	everywhereFor := func(local string) []string {
		if net.ParseIP(local).To4() != nil {
			return everywhereV4
		}
		return everywhereV6
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
		externals = appendUnique(externals, everywhereFor(local)...)
		rules = append(rules, webrtc.ICEAddressRewriteRule{
			External:        externals,
			Local:           local,
			AsCandidateType: webrtc.ICECandidateTypeHost,
			Mode:            webrtc.ICEAddressRewriteReplace,
		})
		handledLocals = append(handledLocals, local)
	}
	if !sharedPort {
		for _, ip := range bareIPs {
			if !slices.Contains(localIPs, ip) || slices.Contains(handledLocals, ip) {
				continue
			}
			// identity rule: advertise this local address as itself (plus the
			// everywhere-valid addresses), shielding it from the catch-all below
			rules = append(rules, webrtc.ICEAddressRewriteRule{
				External:        appendUnique([]string{ip}, everywhereFor(ip)...),
				Local:           ip,
				AsCandidateType: webrtc.ICECandidateTypeHost,
				Mode:            webrtc.ICEAddressRewriteReplace,
			})
			handledLocals = append(handledLocals, ip)
		}
	}

	// per-family catch-all for every other local socket, or a drop rule when
	// there is nothing of that family valid for them
	rules = append(rules, catchAllOrDropRule(everywhereV4, ipv4Networks, dropSentinelForIPv4Rule))
	rules = append(rules, catchAllOrDropRule(everywhereV6, ipv6Networks, dropSentinelForIPv6Rule))

	return rules, nil
}

// usesSharedPortGathering reports whether host candidates are gathered through
// the UDP/TCP muxes (one port shared by every socket) rather than a per-socket
// ephemeral port range. Mirrors the branch order in rtcconfig.NewWebRTCConfig:
// a configured port range takes precedence over udp_port.
//
// Why the gathering mode decides how bare LOCAL entries are treated: pion
// rewrites a candidate's address but keeps its socket's port. With port-range
// gathering each interface socket has its own port, so advertising interface A's
// address for interface B's socket yields address(A):port(B), where nothing
// listens — hence scoping a bare local entry to its own socket. With mux
// gathering every socket has the same port, so that failure cannot happen, and
// scoping is not merely unnecessary but harmful: "local" comes from interface
// enumeration (GetLocalIPAddresses), which includes addresses the mux never
// binds a socket for — a public IP aliased onto `lo` so the SFU can reach its
// own TURN relay is the motivating case. An identity rule bound to that
// non-existent socket never fires and the address silently disappears from the
// SDP. Advertising every bare entry from every socket is what keeps it there.
func usesSharedPortGathering(rtcConf *config.RTCConfig) bool {
	return rtcConf.ICEPortRangeStart == 0 || rtcConf.ICEPortRangeEnd == 0
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

// disableDiscoveryForAdvertisedIPs turns off STUN-based external-IP discovery
// when rtc.advertised_ips is set, and reports whether it changed anything.
//
// The list is authoritative for candidate advertisement, so discovery could at
// most produce state that applyAdvertisedIPs then has to undo (rewrite rules,
// NAT1To1IPs, STUN servers). But one piece of discovery state cannot be undone
// after the fact: with external_ip_only, getNAT1to1IPsForConf replaces the ICE
// IP filter with "only the IPv4 addresses discovery mapped", and
// rtcconfig.NewWebRTCConfig applies that filter to the UDP mux when it binds
// its sockets. Any local address discovery did not map — such as the local side
// of an explicit external/local pair — then has no socket at all, and no
// rewrite rule can conjure a candidate for it. Skipping discovery keeps the mux
// bound under the operator's rtc.ips filter only, and saves the 1-5 s STUN
// round trip at startup.
//
// Must run on the RTCConfig BEFORE rtcconfig.NewWebRTCConfig. node_ip and its
// auto-generation are independent of these flags.
func disableDiscoveryForAdvertisedIPs(rtcConf *config.RTCConfig) bool {
	if len(rtcConf.AdvertisedIPs) == 0 || !(rtcConf.UseExternalIP || rtcConf.ExternalIPOnly) {
		return false
	}
	logger.Infow("rtc.advertised_ips is set; skipping external IP discovery",
		"use_external_ip", rtcConf.UseExternalIP,
		"external_ip_only", rtcConf.ExternalIPOnly,
	)
	rtcConf.UseExternalIP = false
	rtcConf.ExternalIPOnly = false
	return true
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

	sharedPort := usesSharedPortGathering(rtcConf)
	rules, err := buildAdvertisedIPRules(rtcConf.AdvertisedIPs, localIPs, sharedPort)
	if err != nil {
		return err
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
		"gathering", map[bool]string{true: "shared-port (mux)", false: "port-range"}[sharedPort],
	)

	if err := webRTCConfig.SettingEngine.SetICEAddressRewriteRules(rules...); err != nil {
		return fmt.Errorf("rtc.advertised_ips: %w", err)
	}
	webRTCConfig.NAT1To1IPs = nil
	webRTCConfig.Configuration.ICEServers = nil

	return nil
}

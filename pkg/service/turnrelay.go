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
	"net"
	"net/netip"

	"github.com/pion/turn/v5"
	"github.com/pkg/errors"

	"github.com/livekit/livekit-server/pkg/config"
)

var ErrTURNTCPRelayDisabled = errors.New("TURN TCP relay allocations are disabled")

// restricted ranges not covered by the net.IP predicates
var extraRestrictedPeerPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),      // "this" network
	netip.MustParsePrefix("100.64.0.0/10"),  // carrier-grade NAT
	netip.MustParsePrefix("198.18.0.0/15"),  // benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),    // reserved, includes broadcast
	netip.MustParsePrefix("64:ff9b::/96"),   // NAT64, can map to restricted IPv4
	netip.MustParsePrefix("64:ff9b:1::/48"), // local-use NAT64
}

func isRestrictedPeerIP(ip net.IP) bool {
	if ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() ||
		ip.IsPrivate() ||
		ip.IsUnspecified() {
		return true
	}

	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	addr = addr.Unmap()
	for _, prefix := range extraRestrictedPeerPrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

type portRange struct {
	start uint16
	end   uint16
}

// rejectTCPRelayAddressGenerator refuses RFC 6062 TCP relays, which would let
// a client open TCP connections to any permitted address.
type rejectTCPRelayAddressGenerator struct {
	turn.RelayAddressGenerator
}

func newRejectTCPRelayAddressGenerator(g turn.RelayAddressGenerator) *rejectTCPRelayAddressGenerator {
	return &rejectTCPRelayAddressGenerator{RelayAddressGenerator: g}
}

func (g *rejectTCPRelayAddressGenerator) AllocateListener(turn.AllocateListenerConfig) (net.Listener, net.Addr, error) {
	return nil, nil, ErrTURNTCPRelayDisabled
}

func (g *rejectTCPRelayAddressGenerator) AllocateConn(turn.AllocateConnConfig) (net.Conn, error) {
	return nil, ErrTURNTCPRelayDisabled
}

// turnRelayPolicy enforces what TURN permissions cannot, since they are per IP
// only: relays to this host's own addresses may reach only the RTC media
// ports, because such packets are delivered locally and bypass any firewall
// in front of the host. Used for embedded TURN, which shares the host with
// the SFU.
type turnRelayPolicy struct {
	turn.RelayAddressGenerator

	localIPs   map[netip.Addr]struct{}
	mediaPorts []portRange
}

func newTURNRelayPolicy(g turn.RelayAddressGenerator, localIPs map[netip.Addr]struct{}, mediaPorts []portRange) *turnRelayPolicy {
	return &turnRelayPolicy{
		RelayAddressGenerator: g,
		localIPs:              localIPs,
		mediaPorts:            mediaPorts,
	}
}

func (g *turnRelayPolicy) allows(addr net.Addr) bool {
	udpAddr, ok := addr.(*net.UDPAddr)
	if !ok {
		return false
	}
	ap := udpAddr.AddrPort()
	if _, ok := g.localIPs[ap.Addr().Unmap().WithZone("")]; !ok {
		return true
	}
	for _, r := range g.mediaPorts {
		if ap.Port() >= r.start && ap.Port() <= r.end {
			return true
		}
	}
	return false
}

func (g *turnRelayPolicy) AllocatePacketConn(c turn.AllocateListenerConfig) (net.PacketConn, net.Addr, error) {
	conn, addr, err := g.RelayAddressGenerator.AllocatePacketConn(c)
	if err != nil {
		return conn, addr, err
	}
	return &turnRelayPacketConn{PacketConn: conn, policy: g}, addr, nil
}

// turnRelayPacketConn silently drops relayed packets to peers the policy does
// not allow.
type turnRelayPacketConn struct {
	net.PacketConn
	policy *turnRelayPolicy
}

func (c *turnRelayPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	if !c.policy.allows(addr) {
		return len(p), nil
	}
	return c.PacketConn.WriteTo(p, addr)
}

// getTURNLocalIPs returns the addresses that are delivered to this host: its
// interface addresses and the node IPs, which may be 1:1 NAT addresses.
func getTURNLocalIPs(rtcConf *config.RTCConfig) (map[netip.Addr]struct{}, error) {
	ips := make(map[netip.Addr]struct{})

	ifAddrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	for _, ifAddr := range ifAddrs {
		ipNet, ok := ifAddr.(*net.IPNet)
		if !ok {
			continue
		}
		if addr, ok := netip.AddrFromSlice(ipNet.IP); ok {
			ips[addr.Unmap()] = struct{}{}
		}
	}

	for _, ip := range rtcConf.NodeIP.ToStringSlice() {
		addr, err := netip.ParseAddr(ip)
		if err != nil {
			return nil, err
		}
		ips[addr.Unmap()] = struct{}{}
	}
	return ips, nil
}

func getTURNMediaPorts(rtcConf *config.RTCConfig) []portRange {
	if rtcConf.ForceTCP {
		return nil
	}

	var ports []portRange
	if rtcConf.ICEPortRangeStart != 0 && rtcConf.ICEPortRangeEnd != 0 {
		ports = append(ports, portRange{start: uint16(rtcConf.ICEPortRangeStart), end: uint16(rtcConf.ICEPortRangeEnd)})
	} else if rtcConf.UDPPort.Valid() {
		end := max(rtcConf.UDPPort.End, rtcConf.UDPPort.Start)
		ports = append(ports, portRange{start: uint16(rtcConf.UDPPort.Start), end: uint16(end)})
		if rtcConf.UseStunPortAsICE {
			ports = append(ports, portRange{start: 3478, end: 3478})
		}
	}
	return ports
}

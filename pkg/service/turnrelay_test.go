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
	"fmt"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/pion/turn/v5"
	"github.com/stretchr/testify/require"

	"github.com/livekit/mediatransportutil/pkg/rtcconfig"

	"github.com/livekit/livekit-server/pkg/config"
)

func TestGetTURNMediaPorts(t *testing.T) {
	ports := getTURNMediaPorts(&config.RTCConfig{RTCConfig: rtcconfig.RTCConfig{
		UDPPort:          rtcconfig.PortRange{Start: 7882, End: 7885},
		UseStunPortAsICE: true,
	}})
	require.Equal(t, []portRange{{7882, 7885}, {3478, 3478}}, ports)

	ports = getTURNMediaPorts(&config.RTCConfig{RTCConfig: rtcconfig.RTCConfig{
		UDPPort:           rtcconfig.PortRange{Start: 7882},
		ICEPortRangeStart: 50000,
		ICEPortRangeEnd:   60000,
	}})
	require.Equal(t, []portRange{{50000, 60000}}, ports)

	require.Empty(t, getTURNMediaPorts(&config.RTCConfig{RTCConfig: rtcconfig.RTCConfig{
		UDPPort:  rtcconfig.PortRange{Start: 7882},
		ForceTCP: true,
	}}))
}

func TestGetTURNLocalIPs(t *testing.T) {
	ips, err := getTURNLocalIPs(&config.RTCConfig{RTCConfig: rtcconfig.RTCConfig{
		NodeIP: rtcconfig.NodeIP{V4: "203.0.113.1", V6: "2001:db8::1"},
	}})
	require.NoError(t, err)
	require.Contains(t, ips, netip.MustParseAddr("127.0.0.1"))
	require.Contains(t, ips, netip.MustParseAddr("203.0.113.1"))
	require.Contains(t, ips, netip.MustParseAddr("2001:db8::1"))
}

func TestTURNRelayPolicy_Allows(t *testing.T) {
	policy := newTURNRelayPolicy(nil,
		map[netip.Addr]struct{}{netip.MustParseAddr("203.0.113.1"): {}},
		[]portRange{{7882, 7882}, {50000, 60000}},
	)

	for _, tc := range []struct {
		addr    net.Addr
		allowed bool
	}{
		{&net.UDPAddr{IP: net.ParseIP("203.0.113.1"), Port: 7882}, true},
		{&net.UDPAddr{IP: net.ParseIP("203.0.113.1").To16(), Port: 55000}, true},
		{&net.UDPAddr{IP: net.ParseIP("203.0.113.1"), Port: 22}, false},
		{&net.UDPAddr{IP: net.ParseIP("203.0.113.1"), Port: 443}, false},
		{&net.UDPAddr{IP: net.ParseIP("203.0.113.1"), Port: 60001}, false},
		{&net.UDPAddr{IP: net.ParseIP("198.51.100.7"), Port: 7882}, true},
		{&net.UDPAddr{IP: net.ParseIP("198.51.100.7"), Port: 443}, true},
		{&net.TCPAddr{IP: net.ParseIP("198.51.100.7"), Port: 443}, false},
	} {
		require.Equal(t, tc.allowed, policy.allows(tc.addr), "%s", tc.addr)
	}
}

type fakeRelayAddressGenerator struct {
	conn net.PacketConn
}

func (g *fakeRelayAddressGenerator) Validate() error { return nil }

func (g *fakeRelayAddressGenerator) AllocatePacketConn(turn.AllocateListenerConfig) (net.PacketConn, net.Addr, error) {
	return g.conn, g.conn.LocalAddr(), nil
}

func (g *fakeRelayAddressGenerator) AllocateListener(turn.AllocateListenerConfig) (net.Listener, net.Addr, error) {
	panic("must not be called")
}

func (g *fakeRelayAddressGenerator) AllocateConn(turn.AllocateConnConfig) (net.Conn, error) {
	panic("must not be called")
}

func listenUDP(t *testing.T) net.PacketConn {
	t.Helper()
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestTURNRelayPolicy_PacketConn(t *testing.T) {
	relay := listenUDP(t)
	mediaPeer := listenUDP(t)
	otherPeer := listenUDP(t)

	mediaPort := uint16(mediaPeer.LocalAddr().(*net.UDPAddr).Port)
	policy := newTURNRelayPolicy(
		&fakeRelayAddressGenerator{conn: relay},
		map[netip.Addr]struct{}{netip.MustParseAddr("127.0.0.1"): {}},
		[]portRange{{mediaPort, mediaPort}},
	)

	conn, _, err := policy.AllocatePacketConn(turn.AllocateListenerConfig{UserID: "PA_x"})
	require.NoError(t, err)

	buf := make([]byte, 64)

	// writes to a non-media port on this host are dropped
	n, err := conn.WriteTo([]byte("denied"), otherPeer.LocalAddr())
	require.NoError(t, err)
	require.Equal(t, len("denied"), n)
	require.NoError(t, otherPeer.SetReadDeadline(time.Now().Add(100*time.Millisecond)))
	_, _, err = otherPeer.ReadFrom(buf)
	require.Error(t, err)

	_, err = conn.WriteTo([]byte("allowed"), mediaPeer.LocalAddr())
	require.NoError(t, err)
	require.NoError(t, mediaPeer.SetReadDeadline(time.Now().Add(2*time.Second)))
	n, _, err = mediaPeer.ReadFrom(buf)
	require.NoError(t, err)
	require.Equal(t, "allowed", string(buf[:n]))
}

func TestRejectTCPRelayAddressGenerator(t *testing.T) {
	relay := listenUDP(t)
	g := newRejectTCPRelayAddressGenerator(&fakeRelayAddressGenerator{conn: relay})

	_, _, err := g.AllocateListener(turn.AllocateListenerConfig{})
	require.ErrorIs(t, err, ErrTURNTCPRelayDisabled)
	_, err = g.AllocateConn(turn.AllocateConnConfig{})
	require.ErrorIs(t, err, ErrTURNTCPRelayDisabled)

	conn, _, err := g.AllocatePacketConn(turn.AllocateListenerConfig{})
	require.NoError(t, err)
	require.Same(t, relay, conn)
}

// allocateTestTurnRelay starts a TURN server on loopback, with the RTC media
// port set to mediaPort, and returns a relay allocated through it.
func allocateTestTurnRelay(t *testing.T, standalone bool, mediaPort int) net.PacketConn {
	t.Helper()

	turnPort := listenUDP(t)
	turnAddr := turnPort.LocalAddr().String()
	require.NoError(t, turnPort.Close())

	conf := &config.Config{
		RTC: config.RTCConfig{RTCConfig: rtcconfig.RTCConfig{
			NodeIP:  rtcconfig.NodeIP{V4: "127.0.0.1"},
			UDPPort: rtcconfig.PortRange{Start: mediaPort},
		}},
		TURN: config.TURNConfig{
			Enabled:                  true,
			UDPPort:                  turnPort.LocalAddr().(*net.UDPAddr).Port,
			BindAddresses:            []string{"127.0.0.1"},
			RelayPortRangeStart:      40000,
			RelayPortRangeEnd:        40100,
			AllowRestrictedPeerCIDRs: []string{"127.0.0.0/8"},
		},
	}
	h := newTestTurnAuthHandler()
	server, err := NewTurnServer(conf, h.HandleAuth, standalone)
	require.NoError(t, err)
	t.Cleanup(func() { _ = server.Close() })

	username, expiry := h.CreateUsername(turnTestAPIKey, "PA_relay", 300)
	password, err := h.CreatePassword(turnTestAPIKey, "PA_relay", expiry)
	require.NoError(t, err)

	client, err := turn.NewClient(&turn.ClientConfig{
		STUNServerAddr: turnAddr,
		TURNServerAddr: turnAddr,
		Username:       username,
		Password:       password,
		Realm:          LivekitRealm,
		Conn:           listenUDP(t),
	})
	require.NoError(t, err)
	t.Cleanup(client.Close)
	require.NoError(t, client.Listen())

	relayConn, err := client.Allocate()
	require.NoError(t, err)
	t.Cleanup(func() { _ = relayConn.Close() })
	return relayConn
}

func TestNewTurnServer_RelayPolicy(t *testing.T) {
	mediaPeer := listenUDP(t)
	otherPeer := listenUDP(t)
	relayConn := allocateTestTurnRelay(t, false, mediaPeer.LocalAddr().(*net.UDPAddr).Port)

	buf := make([]byte, 64)

	_, err := relayConn.WriteTo([]byte("denied"), otherPeer.LocalAddr())
	require.NoError(t, err)
	_, err = relayConn.WriteTo([]byte("allowed"), mediaPeer.LocalAddr())
	require.NoError(t, err)

	require.NoError(t, mediaPeer.SetReadDeadline(time.Now().Add(2*time.Second)))
	n, relayAddr, err := mediaPeer.ReadFrom(buf)
	require.NoError(t, err)
	require.Equal(t, "allowed", string(buf[:n]))

	require.NoError(t, otherPeer.SetReadDeadline(time.Now().Add(200*time.Millisecond)))
	_, _, err = otherPeer.ReadFrom(buf)
	require.Error(t, err, "relay to a non-media port on this host must be dropped")

	// the media peer can reply through the relay
	_, err = mediaPeer.WriteTo([]byte("reply"), relayAddr)
	require.NoError(t, err)
	require.NoError(t, relayConn.SetReadDeadline(time.Now().Add(2*time.Second)))
	n, from, err := relayConn.ReadFrom(buf)
	require.NoError(t, err)
	require.Equal(t, "reply", string(buf[:n]))
	require.Equal(t, mediaPeer.LocalAddr().String(), from.String())
}

func TestNewTurnServer_StandaloneRelayPolicy(t *testing.T) {
	mediaPeer := listenUDP(t)
	otherPeer := listenUDP(t)
	relayConn := allocateTestTurnRelay(t, true, mediaPeer.LocalAddr().(*net.UDPAddr).Port)

	// standalone TURN has no SFU on the host, so ports on this host are not limited
	_, err := relayConn.WriteTo([]byte("allowed"), otherPeer.LocalAddr())
	require.NoError(t, err)

	buf := make([]byte, 64)
	require.NoError(t, otherPeer.SetReadDeadline(time.Now().Add(2*time.Second)))
	n, _, err := otherPeer.ReadFrom(buf)
	require.NoError(t, err)
	require.Equal(t, "allowed", string(buf[:n]))
}

func TestNewTurnServer_RejectsTCPRelay(t *testing.T) {
	for _, standalone := range []bool{false, true} {
		t.Run(fmt.Sprintf("standalone=%v", standalone), func(t *testing.T) {
			ln, err := net.Listen("tcp4", "127.0.0.1:0")
			require.NoError(t, err)
			turnAddr := ln.Addr().String()
			require.NoError(t, ln.Close())

			conf := &config.Config{
				RTC: config.RTCConfig{RTCConfig: rtcconfig.RTCConfig{
					NodeIP:  rtcconfig.NodeIP{V4: "127.0.0.1"},
					UDPPort: rtcconfig.PortRange{Start: 7882},
				}},
				TURN: config.TURNConfig{
					Enabled:                  true,
					Domain:                   "turn.example.com",
					TLSPort:                  ln.Addr().(*net.TCPAddr).Port,
					ExternalTLS:              true,
					BindAddresses:            []string{"127.0.0.1"},
					RelayPortRangeStart:      40000,
					RelayPortRangeEnd:        40100,
					AllowRestrictedPeerCIDRs: []string{"127.0.0.0/8"},
				},
			}
			h := newTestTurnAuthHandler()
			server, err := NewTurnServer(conf, h.HandleAuth, standalone)
			require.NoError(t, err)
			t.Cleanup(func() { _ = server.Close() })

			username, expiry := h.CreateUsername(turnTestAPIKey, "PA_tcp", 300)
			password, err := h.CreatePassword(turnTestAPIKey, "PA_tcp", expiry)
			require.NoError(t, err)

			tcpConn, err := net.Dial("tcp4", turnAddr)
			require.NoError(t, err)
			client, err := turn.NewClient(&turn.ClientConfig{
				STUNServerAddr: turnAddr,
				TURNServerAddr: turnAddr,
				Username:       username,
				Password:       password,
				Realm:          LivekitRealm,
				Conn:           turn.NewSTUNConn(tcpConn),
			})
			require.NoError(t, err)
			t.Cleanup(client.Close)
			require.NoError(t, client.Listen())

			_, err = client.AllocateTCP()
			require.Error(t, err, "TCP relay allocation must be refused")

			// a UDP relay over the same TCP connection still works
			relayConn, err := client.Allocate()
			require.NoError(t, err)
			_ = relayConn.Close()
		})
	}
}

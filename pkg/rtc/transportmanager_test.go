// Copyright 2023 LiveKit, Inc.
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
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/rtc/types"
)

func TestShortFailureFallback(t *testing.T) {
	const (
		none = livekit.ICECandidateType_ICT_NONE
		tcp  = livekit.ICECandidateType_ICT_TCP
		tls  = livekit.ICECandidateType_ICT_TLS

		unknown = types.ICEConnectionTypeUnknown
		udpPair = types.ICEConnectionTypeUDP
		tcpPair = types.ICEConnectionTypeTCP
	)
	cases := []struct {
		name        string
		tcpListener bool
		turns       bool
		publisher   types.ICEConnectionType
		subscriber  types.ICEConnectionType
		// connection type of the transport that failed
		failed types.ICEConnectionType
		// the failure came soon after the transport connected, or from the TCP connect timer
		short    bool
		current  livekit.ICECandidateType
		expected livekit.ICECandidateType
	}{
		{"UDP failed, node offers ICE/TCP", true, true, udpPair, udpPair, udpPair, true, none, tcp},
		{"UDP failed, no ICE/TCP, TURN/TLS on", false, true, udpPair, udpPair, udpPair, true, none, tls},
		{"UDP failed, no ICE/TCP, no TURN/TLS", false, false, udpPair, udpPair, udpPair, true, none, none},
		{"TCP failed, TURN/TLS on", true, true, tcpPair, tcpPair, tcpPair, true, tcp, tls},
		{"TCP failed, no TURN/TLS", true, false, tcpPair, tcpPair, tcpPair, true, tcp, tcp},
		{"TCP never connected, TCP connect timer", true, true, unknown, unknown, unknown, true, tcp, tls},
		{"TCP never connected, ICE failure", true, true, unknown, unknown, unknown, false, tcp, tls},
		{"TCP never connected, no TURN/TLS", true, false, unknown, unknown, unknown, false, tcp, none},
		{"nothing connected, no preference", true, true, unknown, unknown, unknown, false, none, none},
		{"TURN/TLS never connected", true, true, unknown, unknown, unknown, false, tls, none},
		// the subscriber restarted in TCP mode, the publisher still has the UDP pair from before the preference
		{"TCP restart never connected, other transport on UDP", true, true, udpPair, unknown, unknown, false, tcp, tls},
		{"TCP restart never connected, other transport on UDP, no TURN/TLS", true, false, udpPair, unknown, unknown, false, tcp, none},
		{"old UDP pair failed after a TCP preference", true, true, udpPair, unknown, udpPair, true, tcp, tcp},
		{"old UDP pair failed after a TLS preference", false, true, udpPair, unknown, udpPair, true, tls, tls},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newParticipantForTestWithOpts("test", &participantOpts{
				protocolVersion: 17,
				clientInfo:      &livekit.ClientInfo{Sdk: livekit.ClientInfo_JS, Version: "2.15.2"},
			})
			defer p.Close(false, types.ParticipantCloseReasonNone, false)

			tm := p.TransportManager
			tm.params.AllowTCPFallback = true
			tm.params.TURNSEnabled = c.turns
			if c.tcpListener {
				tm.params.Config.TCPMuxListener = &net.TCPListener{}
			}
			tm.publisher.connectionDetails.Type = c.publisher
			tm.subscriber.connectionDetails.Type = c.subscriber
			p.UpdateLastSeenSignal()
			p.SetSignalSourceValid(true)
			p.SetICEConfig(&livekit.ICEConfig{PreferenceSubscriber: c.current, PreferencePublisher: c.current})

			tm.handleConnectionFailed(c.short, c.failed)
			require.Equal(t, c.expected, p.GetICEConfig().GetPreferenceSubscriber())
		})
	}
}

func TestClientReconnectFallback(t *testing.T) {
	const (
		tcp = livekit.ICECandidateType_ICT_TCP
		tls = livekit.ICECandidateType_ICT_TLS
	)
	// the subscriber restarted in TCP mode and did not connect, the publisher has the UDP pair from before
	setup := func(t *testing.T) (*ParticipantImpl, *TransportManager) {
		p := newParticipantForTestWithOpts("test", &participantOpts{
			protocolVersion: 17,
			clientInfo:      &livekit.ClientInfo{Sdk: livekit.ClientInfo_JS, Version: "2.15.2"},
		})
		t.Cleanup(func() { p.Close(false, types.ParticipantCloseReasonNone, false) })

		tm := p.TransportManager
		tm.params.AllowTCPFallback = true
		tm.params.TURNSEnabled = true
		tm.params.Config.TCPMuxListener = &net.TCPListener{}
		tm.publisher.connectionDetails.Type = types.ICEConnectionTypeUDP
		tm.subscriber.connectionDetails.Type = types.ICEConnectionTypeUnknown
		for _, pc := range []*PCTransport{tm.publisher, tm.subscriber} {
			pc.iceFirstConnectedAt = time.Now()
		}
		p.UpdateLastSeenSignal()
		p.SetSignalSourceValid(true)
		p.SetICEConfig(&livekit.ICEConfig{PreferenceSubscriber: tcp, PreferencePublisher: tcp})
		return p, tm
	}

	t.Run("report for the old UDP pair keeps the preference", func(t *testing.T) {
		p, tm := setup(t)
		tm.HandleClientReconnect(livekit.ReconnectReason_RR_PUBLISHER_FAILED)
		require.Equal(t, tcp, p.GetICEConfig().GetPreferenceSubscriber())
	})

	t.Run("report for the transport that did not connect in TCP mode", func(t *testing.T) {
		p, tm := setup(t)
		tm.HandleClientReconnect(livekit.ReconnectReason_RR_SUBSCRIBER_FAILED)
		require.Equal(t, tls, p.GetICEConfig().GetPreferenceSubscriber())
	})

	t.Run("report after the server chose a fallback for the same failure", func(t *testing.T) {
		p, tm := setup(t)
		tm.handleConnectionFailed(false, types.ICEConnectionTypeUnknown)
		require.Equal(t, tls, p.GetICEConfig().GetPreferenceSubscriber())

		tm.HandleClientReconnect(livekit.ReconnectReason_RR_SUBSCRIBER_FAILED)
		require.Equal(t, tls, p.GetICEConfig().GetPreferenceSubscriber())
	})
}

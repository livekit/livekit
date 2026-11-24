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
		current     livekit.ICECandidateType
		expected    livekit.ICECandidateType
	}{
		{"UDP failed, node offers ICE/TCP", true, true, udpPair, udpPair, none, tcp},
		{"UDP failed, no ICE/TCP, TURN/TLS on", false, true, udpPair, udpPair, none, tls},
		{"UDP failed, no ICE/TCP, no TURN/TLS", false, false, udpPair, udpPair, none, none},
		{"TCP failed, TURN/TLS on", true, true, tcpPair, tcpPair, tcp, tls},
		{"TCP failed, no TURN/TLS", true, false, tcpPair, tcpPair, tcp, tcp},
		// the subscriber restarted in TCP mode and never connected, the publisher still has its UDP pair
		{"TCP restart failed, other transport on UDP", true, true, udpPair, unknown, tcp, none},
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

			tm.handleConnectionFailed(true)
			require.Equal(t, c.expected, p.GetICEConfig().GetPreferenceSubscriber())
		})
	}
}

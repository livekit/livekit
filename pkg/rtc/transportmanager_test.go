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
	)
	cases := []struct {
		name        string
		tcpListener bool
		turns       bool
		failed      types.ICEConnectionType
		current     livekit.ICECandidateType
		expected    livekit.ICECandidateType
	}{
		{"UDP failed, node offers ICE/TCP", true, true, types.ICEConnectionTypeUDP, none, tcp},
		{"UDP failed, no ICE/TCP, TURN/TLS on", false, true, types.ICEConnectionTypeUDP, none, tls},
		{"UDP failed, no ICE/TCP, no TURN/TLS", false, false, types.ICEConnectionTypeUDP, none, none},
		{"UDP pair reported again after a TCP fallback", true, true, types.ICEConnectionTypeUDP, tcp, tcp},
		{"UDP pair reported again after a TLS fallback", false, true, types.ICEConnectionTypeUDP, tls, tls},
		{"TCP failed, TURN/TLS on", true, true, types.ICEConnectionTypeTCP, tcp, tls},
		{"TCP failed, no TURN/TLS", true, false, types.ICEConnectionTypeTCP, tcp, tcp},
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
			for _, pc := range []*PCTransport{tm.publisher, tm.subscriber} {
				pc.connectionDetails.Type = c.failed
			}
			p.UpdateLastSeenSignal()
			p.SetSignalSourceValid(true)
			p.SetICEConfig(&livekit.ICEConfig{PreferenceSubscriber: c.current, PreferencePublisher: c.current})

			tm.handleConnectionFailed(true)
			require.Equal(t, c.expected, p.GetICEConfig().GetPreferenceSubscriber())
		})
	}
}

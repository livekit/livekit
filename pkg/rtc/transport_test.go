package rtc

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v3"
	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/testutils"
)

func TestMissingAnswerDuringICERestart(t *testing.T) {
	params := TransportParams{
		ParticipantID:       "id",
		ParticipantIdentity: "identity",
		Target:              livekit.SignalTarget_PUBLISHER,
		Config:              &WebRTCConfig{},
	}
	transportA, err := NewPCTransport(params)
	require.NoError(t, err)
	_, err = transportA.pc.CreateDataChannel("test", nil)
	require.NoError(t, err)
	transportB, err := NewPCTransport(params)
	require.NoError(t, err)

	// exchange ICE
	handleICEExchange(t, transportA, transportB)

	// set offer/answer
	handleOffer := handleOfferFunc(t, transportA, transportB)
	transportA.OnOffer(handleOffer)

	// first establish connection
	transportA.Negotiate(true)

	// ensure we are connected the first time
	testutils.WithTimeout(t, func() string {
		if transportA.pc.ICEConnectionState() != webrtc.ICEConnectionStateConnected {
			return "transportA did not become connected"
		}

		if transportB.pc.ICEConnectionState() != webrtc.ICEConnectionStateConnected {
			return "transportB did not become connected"
		}
		return ""
	})
	require.Equal(t, webrtc.ICEConnectionStateConnected, transportA.pc.ICEConnectionState())
	require.Equal(t, webrtc.ICEConnectionStateConnected, transportB.pc.ICEConnectionState())

	// offer again, but missed
	transportA.OnOffer(func(sd webrtc.SessionDescription) error { return nil })
	transportA.Negotiate(true)
	require.Equal(t, webrtc.SignalingStateHaveLocalOffer, transportA.pc.SignalingState())
	require.Equal(t, negotiationStateClient, transportA.negotiationState)

	// now restart ICE
	t.Logf("creating offer with ICE restart")
	transportA.OnOffer(handleOffer)
	transportA.ICERestart()

	testutils.WithTimeout(t, func() string {
		if transportA.pc.ICEConnectionState() != webrtc.ICEConnectionStateConnected {
			return "transportA did not reconnect after ICE restart"
		}
		if transportB.pc.ICEConnectionState() != webrtc.ICEConnectionStateConnected {
			return "transportB did not reconnect after ICE restart"
		}
		return ""
	})
}

func TestNegotiationTiming(t *testing.T) {
	params := TransportParams{
		ParticipantID:       "id",
		ParticipantIdentity: "identity",
		Target:              livekit.SignalTarget_SUBSCRIBER,
		Config:              &WebRTCConfig{},
	}
	transportA, err := NewPCTransport(params)
	require.NoError(t, err)
	_, err = transportA.pc.CreateDataChannel("test", nil)
	require.NoError(t, err)
	transportB, err := NewPCTransport(params)
	require.NoError(t, err)
	require.False(t, transportA.IsEstablished())
	require.False(t, transportB.IsEstablished())

	handleICEExchange(t, transportA, transportB)
	offer := atomic.Value{}
	transportA.OnOffer(func(sd webrtc.SessionDescription) error {
		offer.Store(&sd)
		return nil
	})

	// initial offer
	transportA.Negotiate(true)
	require.Equal(t, negotiationStateClient, transportA.negotiationState)

	// second try, should've flipped transport status to retry
	transportA.Negotiate(true)
	require.Equal(t, negotiationRetry, transportA.negotiationState)

	// third try, should've stayed at retry
	transportA.Negotiate(true)
	require.Equal(t, negotiationRetry, transportA.negotiationState)

	time.Sleep(5 * time.Millisecond)
	actualOffer, ok := offer.Load().(*webrtc.SessionDescription)

	require.True(t, ok)
	transportB.HandleRemoteDescription(*actualOffer)
	answer, err := transportB.pc.CreateAnswer(nil)
	require.NoError(t, err)
	require.NoError(t, transportB.pc.SetLocalDescription(answer))
	transportA.HandleRemoteDescription(answer)

	testutils.WithTimeout(t, func() string {
		if !transportA.IsEstablished() {
			return "transportA is not established"
		}
		if !transportB.IsEstablished() {
			return "transportB is not established"
		}
		return ""
	})

	// it should still be negotiating again
	require.Equal(t, negotiationStateClient, transportA.negotiationState)
	offer2, ok := offer.Load().(*webrtc.SessionDescription)
	require.True(t, ok)
	require.False(t, offer2 == actualOffer)
}

func TestFirstOfferMissedDuringICERestart(t *testing.T) {
	params := TransportParams{
		ParticipantID:       "id",
		ParticipantIdentity: "identity",
		Target:              livekit.SignalTarget_PUBLISHER,
		Config:              &WebRTCConfig{},
	}
	transportA, err := NewPCTransport(params)
	require.NoError(t, err)
	_, err = transportA.pc.CreateDataChannel("test", nil)
	require.NoError(t, err)
	transportB, err := NewPCTransport(params)
	require.NoError(t, err)

	//first offer missed
	transportA.OnOffer(func(sd webrtc.SessionDescription) error { return nil })
	transportA.Negotiate(true)

	// exchange ICE
	handleICEExchange(t, transportA, transportB)

	// set offer/answer with restart ICE, will negotiate twice,
	// first one is recover from missed offer
	// second one is restartICE
	handleOffer := handleOfferFunc(t, transportA, transportB)
	var offerCount int32
	transportA.OnOffer(func(sd webrtc.SessionDescription) error {
		atomic.AddInt32(&offerCount, 1)
		// the second offer is a ice restart offer, so we wait transportB complete the ice gathering
		if transportB.pc.ICEGatheringState() == webrtc.ICEGatheringStateGathering {
			require.Eventually(t, func() bool {
				return transportB.pc.ICEGatheringState() == webrtc.ICEGatheringStateComplete
			}, 10*time.Second, time.Millisecond*10)
		}
		return handleOffer(sd)
	})

	// first establish connection
	transportA.ICERestart()

	// ensure we are connected
	require.Eventually(t, func() bool {
		return transportA.pc.ICEConnectionState() == webrtc.ICEConnectionStateConnected &&
			transportB.pc.ICEConnectionState() == webrtc.ICEConnectionStateConnected &&
			atomic.LoadInt32(&offerCount) == 2
	}, testutils.ConnectTimeout, 10*time.Millisecond, "transport did not connect")
	transportA.Close()
	transportB.Close()
}

func TestFirstAnwserMissedDuringICERestart(t *testing.T) {
	params := TransportParams{
		ParticipantID:       "id",
		ParticipantIdentity: "identity",
		Target:              livekit.SignalTarget_PUBLISHER,
		Config:              &WebRTCConfig{},
	}
	transportA, err := NewPCTransport(params)
	require.NoError(t, err)
	_, err = transportA.pc.CreateDataChannel("test", nil)
	require.NoError(t, err)
	transportB, err := NewPCTransport(params)
	require.NoError(t, err)

	//first anwser missed
	transportA.OnOffer(func(sd webrtc.SessionDescription) error {
		transportB.HandleRemoteDescription(sd)
		answer, err := transportB.pc.CreateAnswer(nil)
		require.NoError(t, err)
		require.NoError(t, transportB.pc.SetLocalDescription(answer))
		return nil
	})
	// exchange ICE
	handleICEExchange(t, transportA, transportB)

	transportA.Negotiate(true)
	require.Eventually(t, func() bool {
		return transportB.pc.SignalingState() == webrtc.SignalingStateStable
	}, time.Second, 10*time.Millisecond)

	// set offer/answer with restart ICE, will negotiate twice,
	// first one is recover from missed offer
	// second one is restartICE
	handleOffer := handleOfferFunc(t, transportA, transportB)
	var offerCount int32
	transportA.OnOffer(func(sd webrtc.SessionDescription) error {
		atomic.AddInt32(&offerCount, 1)
		// the second offer is a ice restart offer, so we wait transportB complete the ice gathering
		if transportB.pc.ICEGatheringState() == webrtc.ICEGatheringStateGathering {
			require.Eventually(t, func() bool {
				return transportB.pc.ICEGatheringState() == webrtc.ICEGatheringStateComplete
			}, 10*time.Second, time.Millisecond*10)
		}
		return handleOffer(sd)
	})

	// first establish connection
	transportA.ICERestart()

	// ensure we are connected
	require.Eventually(t, func() bool {
		return transportA.pc.ICEConnectionState() == webrtc.ICEConnectionStateConnected &&
			transportB.pc.ICEConnectionState() == webrtc.ICEConnectionStateConnected &&
			atomic.LoadInt32(&offerCount) == 2
	}, testutils.ConnectTimeout, 10*time.Millisecond, "transport did not connect")
	transportA.Close()
	transportB.Close()
}

func TestNegotiationFailed(t *testing.T) {
	params := TransportParams{
		ParticipantID:       "id",
		ParticipantIdentity: "identity",
		Target:              livekit.SignalTarget_PUBLISHER,
		Config:              &WebRTCConfig{},
	}
	transportA, err := NewPCTransport(params)
	require.NoError(t, err)

	transportA.OnOffer(func(sd webrtc.SessionDescription) error { return nil })
	var failed int32
	transportA.OnNegotiationFailed(func() {
		atomic.AddInt32(&failed, 1)
	})
	transportA.Negotiate(true)
	require.Eventually(t, func() bool {
		return atomic.LoadInt32(&failed) == 1
	}, negotiationFailedTimeout+time.Second, 10*time.Millisecond, "negotiation failed")
}

func TestFilteringCandidates(t *testing.T) {
	params := TransportParams{
		ParticipantID:       "id",
		ParticipantIdentity: "identity",
		Target:              livekit.SignalTarget_PUBLISHER,
		Config:              &WebRTCConfig{},
		EnabledCodecs: []*livekit.Codec{
			{Mime: webrtc.MimeTypeOpus},
			{Mime: webrtc.MimeTypeVP8},
			{Mime: webrtc.MimeTypeH264},
		},
	}
	transport, err := NewPCTransport(params)
	require.NoError(t, err)

	_, err = transport.pc.CreateDataChannel("test", nil)
	require.NoError(t, err)

	_, err = transport.pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio)
	require.NoError(t, err)

	_, err = transport.pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo)
	require.NoError(t, err)

	offer, err := transport.pc.CreateOffer(nil)
	require.NoError(t, err)

	offerGatheringComplete := webrtc.GatheringCompletePromise(transport.pc)
	require.NoError(t, transport.pc.SetLocalDescription(offer))
	<-offerGatheringComplete

	// should not filter out UDP candidates if TCP is not preferred
	offer = *transport.pc.LocalDescription()
	filteredOffer := transport.filterCandidates(offer)
	require.EqualValues(t, offer.SDP, filteredOffer.SDP)

	parsed, err := offer.Unmarshal()
	require.NoError(t, err)

	// add a couple of TCP candidates
	done := false
	for _, m := range parsed.MediaDescriptions {
		for _, a := range m.Attributes {
			if a.Key == sdp.AttrKeyCandidate {
				for idx, aa := range m.Attributes {
					if aa.Key == sdp.AttrKeyEndOfCandidates {
						modifiedAttributes := make([]sdp.Attribute, idx)
						copy(modifiedAttributes, m.Attributes[:idx])
						modifiedAttributes = append(modifiedAttributes, []sdp.Attribute{
							{
								Key:   sdp.AttrKeyCandidate,
								Value: "054225987 1 tcp 2124414975 159.203.70.248 7881 typ host tcptype passive",
							},
							{
								Key:   sdp.AttrKeyCandidate,
								Value: "054225987 2 tcp 2124414975 159.203.70.248 7881 typ host tcptype passive",
							},
						}...)
						m.Attributes = append(modifiedAttributes, m.Attributes[idx:]...)
						done = true
						break
					}
				}
			}
			if done {
				break
			}
		}
		if done {
			break
		}
	}
	bytes, err := parsed.Marshal()
	require.NoError(t, err)
	offer.SDP = string(bytes)

	parsed, err = offer.Unmarshal()
	require.NoError(t, err)

	getNumTransportTypeCandidates := func(sd *sdp.SessionDescription) (int, int) {
		numUDPCandidates := 0
		numTCPCandidates := 0
		for _, a := range sd.Attributes {
			if a.Key == sdp.AttrKeyCandidate {
				if strings.Contains(a.Value, "udp") {
					numUDPCandidates++
				}
				if strings.Contains(a.Value, "tcp") {
					numTCPCandidates++
				}
			}
		}
		for _, m := range sd.MediaDescriptions {
			for _, a := range m.Attributes {
				if a.Key == sdp.AttrKeyCandidate {
					if strings.Contains(a.Value, "udp") {
						numUDPCandidates++
					}
					if strings.Contains(a.Value, "tcp") {
						numTCPCandidates++
					}
				}
			}
		}
		return numUDPCandidates, numTCPCandidates
	}
	udp, tcp := getNumTransportTypeCandidates(parsed)
	require.NotZero(t, udp)
	require.Equal(t, 2, tcp)

	transport.SetPreferTCP(true)
	filteredOffer = transport.filterCandidates(offer)
	parsed, err = filteredOffer.Unmarshal()
	require.NoError(t, err)
	udp, tcp = getNumTransportTypeCandidates(parsed)
	require.Zero(t, udp)
	require.Equal(t, 2, tcp)
}

func handleOfferFunc(t *testing.T, current, other *PCTransport) func(sd webrtc.SessionDescription) error {
	return func(sd webrtc.SessionDescription) error {
		t.Logf("handling offer")
		t.Logf("setting other remote description")
		other.HandleRemoteDescription(sd)
		answer, err := other.pc.CreateAnswer(nil)
		require.NoError(t, err)
		require.NoError(t, other.pc.SetLocalDescription(answer))

		t.Logf("setting answer on current")
		current.HandleRemoteDescription(answer)

		return nil
	}
}

func handleICEExchange(t *testing.T, a, b *PCTransport) {
	a.pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		t.Logf("got ICE candidate from A: %v", candidate)
		b.AddICECandidate(candidate.ToJSON())
	})
	b.pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		t.Logf("got ICE candidate from B: %v", candidate)
		a.AddICECandidate(candidate.ToJSON())
	})
}

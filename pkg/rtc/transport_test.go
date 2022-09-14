package rtc

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v3"
	"github.com/stretchr/testify/require"
	"go.uber.org/atomic"

	"github.com/livekit/livekit-server/pkg/testutils"
	"github.com/livekit/protocol/livekit"
)

func TestMissingAnswerDuringICERestart(t *testing.T) {
	params := TransportParams{
		ParticipantID:       "id",
		ParticipantIdentity: "identity",
		Config:              &WebRTCConfig{},
		IsOfferer:           true,
	}
	transportA, err := NewPCTransport(params)
	require.NoError(t, err)
	_, err = transportA.pc.CreateDataChannel("test", nil)
	require.NoError(t, err)

	paramsB := params
	paramsB.IsOfferer = false
	transportB, err := NewPCTransport(paramsB)
	require.NoError(t, err)

	// exchange ICE
	handleICEExchange(t, transportA, transportB)

	connectTransports(t, transportA, transportB, false, 1, 1)
	require.Equal(t, webrtc.ICEConnectionStateConnected, transportA.pc.ICEConnectionState())
	require.Equal(t, webrtc.ICEConnectionStateConnected, transportB.pc.ICEConnectionState())

	var negotiationState atomic.Value
	transportA.OnNegotiationStateChanged(func(state NegotiationState) {
		negotiationState.Store(state)
	})

	// offer again, but missed
	var offerReceived atomic.Bool
	transportA.OnOffer(func(sd webrtc.SessionDescription) error {
		require.Equal(t, webrtc.SignalingStateHaveLocalOffer, transportA.pc.SignalingState())
		require.Equal(t, NegotiationStateRemote, negotiationState.Load().(NegotiationState))
		offerReceived.Store(true)
		return nil
	})
	transportA.Negotiate(true)
	require.Eventually(t, func() bool {
		return offerReceived.Load()
	}, 10*time.Second, time.Millisecond*10, "transportA offer not received")

	connectTransports(t, transportA, transportB, true, 1, 1)
	require.Equal(t, webrtc.ICEConnectionStateConnected, transportA.pc.ICEConnectionState())
	require.Equal(t, webrtc.ICEConnectionStateConnected, transportB.pc.ICEConnectionState())

	transportA.Close()
	transportB.Close()
}

func TestNegotiationTiming(t *testing.T) {
	params := TransportParams{
		ParticipantID:       "id",
		ParticipantIdentity: "identity",
		Config:              &WebRTCConfig{},
		IsOfferer:           true,
	}
	transportA, err := NewPCTransport(params)
	require.NoError(t, err)
	_, err = transportA.pc.CreateDataChannel("test", nil)
	require.NoError(t, err)

	paramsB := params
	paramsB.IsOfferer = false
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

	var negotiationState atomic.Value
	transportA.OnNegotiationStateChanged(func(state NegotiationState) {
		negotiationState.Store(state)
	})

	// initial offer
	transportA.Negotiate(true)
	require.Eventually(t, func() bool {
		state, ok := negotiationState.Load().(NegotiationState)
		if !ok {
			return false
		}

		return state == NegotiationStateRemote
	}, 10*time.Second, 10*time.Millisecond, "negotiation state does not match NegotiateStateRemote")

	// second try, should've flipped transport status to retry
	transportA.Negotiate(true)
	require.Eventually(t, func() bool {
		state, ok := negotiationState.Load().(NegotiationState)
		if !ok {
			return false
		}

		return state == NegotiationStateRetry
	}, 10*time.Second, 10*time.Millisecond, "negotiation state does not match NegotiateStateRetry")

	// third try, should've stayed at retry
	transportA.Negotiate(true)
	time.Sleep(100 * time.Millisecond) // some time to process the negotiate event
	require.Eventually(t, func() bool {
		state, ok := negotiationState.Load().(NegotiationState)
		if !ok {
			return false
		}

		return state == NegotiationStateRetry
	}, 10*time.Second, 10*time.Millisecond, "negotiation state does not match NegotiateStateRetry")

	time.Sleep(5 * time.Millisecond)
	actualOffer, ok := offer.Load().(*webrtc.SessionDescription)
	require.True(t, ok)

	transportB.OnAnswer(func(answer webrtc.SessionDescription) error {
		transportA.HandleRemoteDescription(answer)
		return nil
	})
	transportB.HandleRemoteDescription(*actualOffer)

	require.Eventually(t, func() bool {
		return transportA.IsEstablished()
	}, 10*time.Second, time.Millisecond*10, "transportA is not established")
	require.Eventually(t, func() bool {
		return transportB.IsEstablished()
	}, 10*time.Second, time.Millisecond*10, "transportB is not established")

	// it should still be negotiating again
	require.Equal(t, NegotiationStateRemote, negotiationState.Load().(NegotiationState))
	offer2, ok := offer.Load().(*webrtc.SessionDescription)
	require.True(t, ok)
	require.False(t, offer2 == actualOffer)

	transportA.Close()
	transportB.Close()
}

func TestFirstOfferMissedDuringICERestart(t *testing.T) {
	params := TransportParams{
		ParticipantID:       "id",
		ParticipantIdentity: "identity",
		Config:              &WebRTCConfig{},
		IsOfferer:           true,
	}
	transportA, err := NewPCTransport(params)
	require.NoError(t, err)
	_, err = transportA.pc.CreateDataChannel("test", nil)
	require.NoError(t, err)

	paramsB := params
	paramsB.IsOfferer = false
	transportB, err := NewPCTransport(paramsB)
	require.NoError(t, err)

	// exchange ICE
	handleICEExchange(t, transportA, transportB)

	// first offer missed
	var firstOfferReceived atomic.Bool
	transportA.OnOffer(func(sd webrtc.SessionDescription) error {
		firstOfferReceived.Store(true)
		return nil
	})
	transportA.Negotiate(true)
	require.Eventually(t, func() bool {
		return firstOfferReceived.Load()
	}, 10*time.Second, 10*time.Millisecond, "first offer not received")

	// set offer/answer with restart ICE, will negotiate twice,
	// first one is recover from missed offer
	// second one is restartICE
	transportB.OnAnswer(func(answer webrtc.SessionDescription) error {
		transportA.HandleRemoteDescription(answer)
		return nil
	})

	var offerCount atomic.Int32
	transportA.OnOffer(func(sd webrtc.SessionDescription) error {
		offerCount.Inc()

		// the second offer is a ice restart offer, so we wait transportB complete the ice gathering
		if transportB.pc.ICEGatheringState() == webrtc.ICEGatheringStateGathering {
			require.Eventually(t, func() bool {
				return transportB.pc.ICEGatheringState() == webrtc.ICEGatheringStateComplete
			}, 10*time.Second, time.Millisecond*10)
		}

		transportB.HandleRemoteDescription(sd)
		return nil
	})

	// first establish connection
	transportA.ICERestart()

	// ensure we are connected
	require.Eventually(t, func() bool {
		return transportA.pc.ICEConnectionState() == webrtc.ICEConnectionStateConnected &&
			transportB.pc.ICEConnectionState() == webrtc.ICEConnectionStateConnected &&
			offerCount.Load() == 2
	}, testutils.ConnectTimeout, 10*time.Millisecond, "transport did not connect")

	transportA.Close()
	transportB.Close()
}

func TestFirstAnswerMissedDuringICERestart(t *testing.T) {
	params := TransportParams{
		ParticipantID:       "id",
		ParticipantIdentity: "identity",
		Config:              &WebRTCConfig{},
		IsOfferer:           true,
	}
	transportA, err := NewPCTransport(params)
	require.NoError(t, err)
	_, err = transportA.pc.CreateDataChannel("test", nil)
	require.NoError(t, err)

	paramsB := params
	paramsB.IsOfferer = false
	transportB, err := NewPCTransport(paramsB)
	require.NoError(t, err)

	// exchange ICE
	handleICEExchange(t, transportA, transportB)

	// first anwser missed
	var firstAnswerReceived atomic.Bool
	transportB.OnAnswer(func(sd webrtc.SessionDescription) error {
		if firstAnswerReceived.Load() {
			transportA.HandleRemoteDescription(sd)
		} else {
			// do not send first answer so that remote misses the first answer
			firstAnswerReceived.Store(true)
		}
		return nil
	})
	transportA.OnOffer(func(sd webrtc.SessionDescription) error {
		transportB.HandleRemoteDescription(sd)
		return nil
	})

	transportA.Negotiate(true)
	require.Eventually(t, func() bool {
		return transportB.pc.SignalingState() == webrtc.SignalingStateStable && firstAnswerReceived.Load()
	}, time.Second, 10*time.Millisecond, "transportB signaling state did not go to stable")

	// set offer/answer with restart ICE, will negotiate twice,
	// first one is recover from missed offer
	// second one is restartICE
	var offerCount atomic.Int32
	transportA.OnOffer(func(sd webrtc.SessionDescription) error {
		offerCount.Inc()

		// the second offer is a ice restart offer, so we wait transportB complete the ice gathering
		if transportB.pc.ICEGatheringState() == webrtc.ICEGatheringStateGathering {
			require.Eventually(t, func() bool {
				return transportB.pc.ICEGatheringState() == webrtc.ICEGatheringStateComplete
			}, 10*time.Second, time.Millisecond*10)
		}

		transportB.HandleRemoteDescription(sd)
		return nil
	})

	// first establish connection
	transportA.ICERestart()

	// ensure we are connected
	require.Eventually(t, func() bool {
		return transportA.pc.ICEConnectionState() == webrtc.ICEConnectionStateConnected &&
			transportB.pc.ICEConnectionState() == webrtc.ICEConnectionStateConnected &&
			offerCount.Load() == 2
	}, testutils.ConnectTimeout, 10*time.Millisecond, "transport did not connect")

	transportA.Close()
	transportB.Close()
}

func TestNegotiationFailed(t *testing.T) {
	params := TransportParams{
		ParticipantID:       "id",
		ParticipantIdentity: "identity",
		Config:              &WebRTCConfig{},
		IsOfferer:           true,
	}
	transportA, err := NewPCTransport(params)
	require.NoError(t, err)

	transportA.OnICECandidate(func(candidate *webrtc.ICECandidate) error {
		if candidate == nil {
			return nil
		}
		t.Logf("got ICE candidate from A: %v", candidate)
		return nil
	})

	transportA.OnOffer(func(sd webrtc.SessionDescription) error { return nil })
	var failed atomic.Int32
	transportA.OnNegotiationFailed(func() {
		failed.Inc()
	})
	transportA.Negotiate(true)
	require.Eventually(t, func() bool {
		return failed.Load() == 1
	}, negotiationFailedTimeout+time.Second, 10*time.Millisecond, "negotiation failed")

	transportA.Close()
}

func TestFilteringCandidates(t *testing.T) {
	params := TransportParams{
		ParticipantID:       "id",
		ParticipantIdentity: "identity",
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
	filteredOffer := transport.filterCandidates(offer, false)
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
	filteredOffer = transport.filterCandidates(offer, true)
	parsed, err = filteredOffer.Unmarshal()
	require.NoError(t, err)
	udp, tcp = getNumTransportTypeCandidates(parsed)
	require.Zero(t, udp)
	require.Equal(t, 2, tcp)

	transport.Close()
}

func handleICEExchange(t *testing.T, a, b *PCTransport) {
	a.OnICECandidate(func(candidate *webrtc.ICECandidate) error {
		if candidate == nil {
			return nil
		}
		t.Logf("got ICE candidate from A: %v", candidate)
		b.AddICECandidate(candidate.ToJSON())
		return nil
	})
	b.OnICECandidate(func(candidate *webrtc.ICECandidate) error {
		if candidate == nil {
			return nil
		}
		t.Logf("got ICE candidate from B: %v", candidate)
		a.AddICECandidate(candidate.ToJSON())
		return nil
	})
}

func connectTransports(t *testing.T, offerer, answerer *PCTransport, isICERestart bool, expectedOfferCount int32, expectedAnswerCount int32) {
	var offerCount atomic.Int32
	var answerCount atomic.Int32
	answerer.OnAnswer(func(answer webrtc.SessionDescription) error {
		answerCount.Inc()
		offerer.HandleRemoteDescription(answer)
		return nil
	})

	offerer.OnOffer(func(offer webrtc.SessionDescription) error {
		offerCount.Inc()
		answerer.HandleRemoteDescription(offer)
		return nil
	})

	if isICERestart {
		offerer.ICERestart()
	} else {
		offerer.Negotiate(true)
	}

	require.Eventually(t, func() bool {
		return offerCount.Load() == expectedOfferCount
	}, 10*time.Second, time.Millisecond*10, fmt.Sprintf("offer count mismatch, expected: %d, actual: %d", expectedOfferCount, offerCount.Load()))

	require.Eventually(t, func() bool {
		return offerer.pc.ICEConnectionState() == webrtc.ICEConnectionStateConnected
	}, 10*time.Second, time.Millisecond*10, "offerer did not become connected")

	require.Eventually(t, func() bool {
		return answerCount.Load() == expectedAnswerCount
	}, 10*time.Second, time.Millisecond*10, fmt.Sprintf("answer count mismatch, expected: %d, actual: %d", expectedAnswerCount, answerCount.Load()))

	require.Eventually(t, func() bool {
		return answerer.pc.ICEConnectionState() == webrtc.ICEConnectionStateConnected
	}, 10*time.Second, time.Millisecond*10, "answerer did not become connected")
}

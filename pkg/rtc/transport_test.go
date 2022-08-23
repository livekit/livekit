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

	paramsB := params
	paramsB.Target = livekit.SignalTarget_SUBSCRIBER
	transportB, err := NewPCTransport(paramsB)
	require.NoError(t, err)

	// exchange ICE
	handleICEExchange(t, transportA, transportB)

	connectTransports(t, transportA, transportB, false, 1, 1)
	require.Equal(t, webrtc.ICEConnectionStateConnected, transportA.pc.ICEConnectionState())
	require.Equal(t, webrtc.ICEConnectionStateConnected, transportB.pc.ICEConnectionState())

	// offer again, but missed
	var offerReceived atomic.Bool
	transportA.OnOffer(func(sd webrtc.SessionDescription) error {
		t.Logf("got offer 1") // REMOVE
		require.Equal(t, webrtc.SignalingStateHaveLocalOffer, transportA.pc.SignalingState())
		require.Equal(t, negotiationStateClient, transportA.negotiationState)
		offerReceived.Store(true)
		return nil
	})
	transportA.Negotiate(true)
	testutils.WithTimeout(t, func() string {
		if !offerReceived.Load() {
			return "transportA offer not yet received"
		}
		return ""
	})

	connectTransports(t, transportA, transportB, true, 1, 1)
	require.Equal(t, webrtc.ICEConnectionStateConnected, transportA.pc.ICEConnectionState())
	require.Equal(t, webrtc.ICEConnectionStateConnected, transportB.pc.ICEConnectionState())
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
	require.Eventually(t, func() bool {
		return transportA.negotiationState == negotiationStateClient
	}, 10*time.Second, time.Millisecond*10)

	// second try, should've flipped transport status to retry
	transportA.Negotiate(true)
	require.Eventually(t, func() bool {
		return transportA.negotiationState == negotiationRetry
	}, 10*time.Second, time.Millisecond*10)

	// third try, should've stayed at retry
	transportA.Negotiate(true)
	time.Sleep(100 * time.Millisecond) // some time to process the negotiate event
	require.Eventually(t, func() bool {
		return transportA.negotiationState == negotiationRetry
	}, 10*time.Second, time.Millisecond*10)

	time.Sleep(5 * time.Millisecond)
	actualOffer, ok := offer.Load().(*webrtc.SessionDescription)
	require.True(t, ok)

	transportB.OnAnswer(func(answer webrtc.SessionDescription) error {
		transportA.HandleRemoteDescription(answer)
		return nil
	})
	transportB.HandleRemoteDescription(*actualOffer)

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

	paramsB := params
	paramsB.Target = livekit.SignalTarget_SUBSCRIBER
	transportB, err := NewPCTransport(paramsB)
	require.NoError(t, err)

	//first offer missed
	transportA.OnOffer(func(sd webrtc.SessionDescription) error { return nil })
	transportA.Negotiate(true)

	// exchange ICE
	handleICEExchange(t, transportA, transportB)

	// set offer/answer with restart ICE, will negotiate twice,
	// first one is recover from missed offer
	// second one is restartICE
	/* RAJA-TODO
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
	*/
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

	/* RAJA-TODO
	paramsB := params
	paramsB.Target = livekit.SignalTarget_SUBSCRIBER
	transportB, err := NewPCTransport(paramsB)
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
	*/
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
		t.Logf("got offer 2") // REMOVE
		offerCount.Inc()
		answerer.HandleRemoteDescription(offer)
		return nil
	})

	if isICERestart {
		offerer.ICERestart()
	} else {
		offerer.Negotiate(true)
	}

	// ensure we are connected the first time
	testutils.WithTimeout(t, func() string {
		if offerCount.Load() != expectedOfferCount {
			return fmt.Sprintf("offer count mismatch, expected: %d, actual: %d", expectedOfferCount, offerCount.Load())
		}

		if offerer.pc.ICEConnectionState() != webrtc.ICEConnectionStateConnected {
			return "offerer did not become connected"
		}

		if answerCount.Load() != expectedAnswerCount {
			return fmt.Sprintf("answer count mismatch, expected: %d, actual: %d", expectedAnswerCount, answerCount.Load())
		}

		if answerer.pc.ICEConnectionState() != webrtc.ICEConnectionStateConnected {
			return "answerer did not become connected"
		}
		return ""
	})
}

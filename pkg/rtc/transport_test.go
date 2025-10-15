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
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"
	"go.uber.org/atomic"

	"github.com/livekit/livekit-server/pkg/rtc/transport"
	"github.com/livekit/livekit-server/pkg/rtc/transport/transportfakes"
	"github.com/livekit/livekit-server/pkg/sfu/mime"
	"github.com/livekit/livekit-server/pkg/testutils"
	"github.com/livekit/protocol/livekit"
)

func TestMissingAnswerDuringICERestart(t *testing.T) {
	params := TransportParams{
		Config:    &WebRTCConfig{},
		IsOfferer: true,
	}

	paramsA := params
	handlerA := &transportfakes.FakeHandler{}
	paramsA.Handler = handlerA
	transportA, err := NewPCTransport(paramsA)
	require.NoError(t, err)
	_, err = transportA.pc.CreateDataChannel(ReliableDataChannel, nil)
	require.NoError(t, err)

	paramsB := params
	handlerB := &transportfakes.FakeHandler{}
	paramsB.Handler = handlerB
	paramsB.IsOfferer = false
	transportB, err := NewPCTransport(paramsB)
	require.NoError(t, err)

	// exchange ICE
	handleICEExchange(t, transportA, transportB, handlerA, handlerB)

	connectTransports(t, transportA, transportB, handlerA, handlerB, false, 1, 1)
	require.Equal(t, webrtc.ICEConnectionStateConnected, transportA.pc.ICEConnectionState())
	require.Equal(t, webrtc.ICEConnectionStateConnected, transportB.pc.ICEConnectionState())

	var negotiationState atomic.Value
	transportA.OnNegotiationStateChanged(func(state transport.NegotiationState) {
		negotiationState.Store(state)
	})

	// offer again, but missed
	var offerReceived atomic.Bool
	handlerA.OnOfferCalls(func(sd webrtc.SessionDescription, _offerId uint32) error {
		require.Equal(t, webrtc.SignalingStateHaveLocalOffer, transportA.pc.SignalingState())
		require.Equal(t, transport.NegotiationStateRemote, negotiationState.Load().(transport.NegotiationState))
		offerReceived.Store(true)
		return nil
	})
	transportA.Negotiate(true)
	require.Eventually(t, func() bool {
		return offerReceived.Load()
	}, 10*time.Second, time.Millisecond*10, "transportA offer not received")

	connectTransports(t, transportA, transportB, handlerA, handlerB, true, 1, 1)
	require.Equal(t, webrtc.ICEConnectionStateConnected, transportA.pc.ICEConnectionState())
	require.Equal(t, webrtc.ICEConnectionStateConnected, transportB.pc.ICEConnectionState())

	transportA.Close()
	transportB.Close()
}

func TestNegotiationTiming(t *testing.T) {
	params := TransportParams{
		Config:    &WebRTCConfig{},
		IsOfferer: true,
	}

	paramsA := params
	handlerA := &transportfakes.FakeHandler{}
	paramsA.Handler = handlerA
	transportA, err := NewPCTransport(paramsA)
	require.NoError(t, err)
	_, err = transportA.pc.CreateDataChannel(LossyDataChannel, nil)
	require.NoError(t, err)

	paramsB := params
	handlerB := &transportfakes.FakeHandler{}
	paramsB.Handler = handlerB
	paramsB.IsOfferer = false
	transportB, err := NewPCTransport(paramsB)
	require.NoError(t, err)

	require.False(t, transportA.IsEstablished())
	require.False(t, transportB.IsEstablished())

	handleICEExchange(t, transportA, transportB, handlerA, handlerB)
	firstOffer := atomic.Value{}
	firstOfferId := atomic.Uint32{}
	secondOffer := atomic.Value{}
	handlerA.OnOfferCalls(func(sd webrtc.SessionDescription, offerId uint32) error {
		if _, ok := firstOffer.Load().(*webrtc.SessionDescription); !ok {
			firstOffer.Store(&sd)
			firstOfferId.Store(offerId)
		} else {
			secondOffer.Store(&sd)
		}
		return nil
	})

	var negotiationState atomic.Value
	transportA.OnNegotiationStateChanged(func(state transport.NegotiationState) {
		negotiationState.Store(state)
	})

	// initial offer
	transportA.Negotiate(true)
	require.Eventually(t, func() bool {
		state, ok := negotiationState.Load().(transport.NegotiationState)
		if !ok {
			return false
		}

		return state == transport.NegotiationStateRemote
	}, 10*time.Second, 10*time.Millisecond, "negotiation state does not match NegotiateStateRemote")

	// second try, should've flipped transport status to retry
	transportA.Negotiate(true)
	require.Eventually(t, func() bool {
		state, ok := negotiationState.Load().(transport.NegotiationState)
		if !ok {
			return false
		}

		return state == transport.NegotiationStateRetry
	}, 10*time.Second, 10*time.Millisecond, "negotiation state does not match NegotiateStateRetry")

	// third try, should've stayed at retry
	transportA.Negotiate(true)
	time.Sleep(100 * time.Millisecond) // some time to process the negotiate event
	require.Eventually(t, func() bool {
		state, ok := negotiationState.Load().(transport.NegotiationState)
		if !ok {
			return false
		}

		return state == transport.NegotiationStateRetry
	}, 10*time.Second, 10*time.Millisecond, "negotiation state does not match NegotiateStateRetry")

	require.Eventually(t, func() bool {
		_, ok := firstOffer.Load().(*webrtc.SessionDescription)
		if !ok {
			return false
		}
		if firstOfferId.Load() == 0 {
			return false
		}
		return true
	}, 10*time.Second, 10*time.Millisecond, "first offer not received yet")

	handlerB.OnAnswerCalls(func(answer webrtc.SessionDescription, answerId uint32, _midToTrackID map[string]string) error {
		transportA.HandleRemoteDescription(answer, answerId)
		return nil
	})
	transportB.HandleRemoteDescription(*firstOffer.Load().(*webrtc.SessionDescription), firstOfferId.Load())

	require.Eventually(t, func() bool {
		return transportA.IsEstablished()
	}, 10*time.Second, time.Millisecond*10, "transportA is not established")
	require.Eventually(t, func() bool {
		return transportB.IsEstablished()
	}, 10*time.Second, time.Millisecond*10, "transportB is not established")

	// offerer should send another offer after processing the answer
	// as there were forced negotiations a couple of time above
	require.Eventually(t, func() bool {
		state, ok := negotiationState.Load().(transport.NegotiationState)
		if !ok {
			return false
		}

		return state == transport.NegotiationStateRemote
	}, 10*time.Second, 10*time.Millisecond, "negotiation state does not match NegotiateStateRemote")
	_, ok := secondOffer.Load().(*webrtc.SessionDescription)
	require.True(t, ok)

	transportA.Close()
	transportB.Close()
}

func TestFirstOfferMissedDuringICERestart(t *testing.T) {
	params := TransportParams{
		Config:    &WebRTCConfig{},
		IsOfferer: true,
	}

	paramsA := params
	handlerA := &transportfakes.FakeHandler{}
	paramsA.Handler = handlerA
	transportA, err := NewPCTransport(paramsA)
	require.NoError(t, err)
	_, err = transportA.pc.CreateDataChannel(ReliableDataChannel, nil)
	require.NoError(t, err)

	paramsB := params
	handlerB := &transportfakes.FakeHandler{}
	paramsB.Handler = handlerB
	paramsB.IsOfferer = false
	transportB, err := NewPCTransport(paramsB)
	require.NoError(t, err)

	// exchange ICE
	handleICEExchange(t, transportA, transportB, handlerA, handlerB)

	// first offer missed
	var firstOfferReceived atomic.Bool
	handlerA.OnOfferCalls(func(sd webrtc.SessionDescription, _offerId uint32) error {
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
	handlerB.OnAnswerCalls(func(answer webrtc.SessionDescription, answerId uint32, _midToTrackID map[string]string) error {
		transportA.HandleRemoteDescription(answer, answerId)
		return nil
	})

	var offerCount atomic.Int32
	handlerA.OnOfferCalls(func(sd webrtc.SessionDescription, offerId uint32) error {
		offerCount.Inc()

		// the second offer is a ice restart offer, so we wait transportB complete the ice gathering
		if transportB.pc.ICEGatheringState() == webrtc.ICEGatheringStateGathering {
			require.Eventually(t, func() bool {
				return transportB.pc.ICEGatheringState() == webrtc.ICEGatheringStateComplete
			}, 10*time.Second, time.Millisecond*10)
		}

		transportB.HandleRemoteDescription(sd, offerId)
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
		Config:    &WebRTCConfig{},
		IsOfferer: true,
	}

	paramsA := params
	handlerA := &transportfakes.FakeHandler{}
	paramsA.Handler = handlerA
	transportA, err := NewPCTransport(paramsA)
	require.NoError(t, err)
	_, err = transportA.pc.CreateDataChannel(LossyDataChannel, nil)
	require.NoError(t, err)

	paramsB := params
	handlerB := &transportfakes.FakeHandler{}
	paramsB.Handler = handlerB
	paramsB.IsOfferer = false
	transportB, err := NewPCTransport(paramsB)
	require.NoError(t, err)

	// exchange ICE
	handleICEExchange(t, transportA, transportB, handlerA, handlerB)

	// first answer missed
	var firstAnswerReceived atomic.Bool
	handlerB.OnAnswerCalls(func(sd webrtc.SessionDescription, answerId uint32, _midToTrackID map[string]string) error {
		if firstAnswerReceived.Load() {
			transportA.HandleRemoteDescription(sd, answerId)
		} else {
			// do not send first answer so that remote misses the first answer
			firstAnswerReceived.Store(true)
		}
		return nil
	})
	handlerA.OnOfferCalls(func(sd webrtc.SessionDescription, offerId uint32) error {
		transportB.HandleRemoteDescription(sd, offerId)
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
	handlerA.OnOfferCalls(func(sd webrtc.SessionDescription, offerId uint32) error {
		offerCount.Inc()

		// the second offer is a ice restart offer, so we wait for transportB to complete ICE gathering
		if transportB.pc.ICEGatheringState() == webrtc.ICEGatheringStateGathering {
			require.Eventually(t, func() bool {
				return transportB.pc.ICEGatheringState() == webrtc.ICEGatheringStateComplete
			}, 10*time.Second, time.Millisecond*10)
		}

		transportB.HandleRemoteDescription(sd, offerId)
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
		Config:    &WebRTCConfig{},
		IsOfferer: true,
	}

	paramsA := params
	handlerA := &transportfakes.FakeHandler{}
	paramsA.Handler = handlerA
	transportA, err := NewPCTransport(paramsA)
	require.NoError(t, err)
	_, err = transportA.pc.CreateDataChannel(ReliableDataChannel, nil)
	require.NoError(t, err)

	paramsB := params
	handlerB := &transportfakes.FakeHandler{}
	paramsB.Handler = handlerB
	paramsB.IsOfferer = false
	transportB, err := NewPCTransport(paramsB)
	require.NoError(t, err)

	// exchange ICE
	handleICEExchange(t, transportA, transportB, handlerA, handlerB)

	// wait for transport to be connected before maiming the signalling channel
	connectTransports(t, transportA, transportB, handlerA, handlerB, false, 1, 1)

	// reset OnOffer to force a negotiation failure
	handlerA.OnOfferCalls(func(sd webrtc.SessionDescription, offerId uint32) error {
		return nil
	})
	var failed atomic.Int32
	handlerA.OnNegotiationFailedCalls(func() {
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
		Config: &WebRTCConfig{},
		EnabledCodecs: []*livekit.Codec{
			{Mime: mime.MimeTypeOpus.String()},
			{Mime: mime.MimeTypeVP8.String()},
			{Mime: mime.MimeTypeH264.String()},
		},
		Handler: &transportfakes.FakeHandler{},
	}
	transport, err := NewPCTransport(params)
	require.NoError(t, err)

	_, err = transport.pc.CreateDataChannel(ReliableDataChannel, nil)
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
	filteredOffer := transport.filterCandidates(offer, false, true)
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
	filteredOffer = transport.filterCandidates(offer, true, true)
	parsed, err = filteredOffer.Unmarshal()
	require.NoError(t, err)
	udp, tcp = getNumTransportTypeCandidates(parsed)
	require.Zero(t, udp)
	require.Equal(t, 2, tcp)

	transport.Close()
}

func handleICEExchange(t *testing.T, a, b *PCTransport, ah, bh *transportfakes.FakeHandler) {
	ah.OnICECandidateCalls(func(candidate *webrtc.ICECandidate, target livekit.SignalTarget) error {
		if candidate == nil {
			return nil
		}
		t.Logf("got ICE candidate from A: %v", candidate)
		b.AddICECandidate(candidate.ToJSON())
		return nil
	})
	bh.OnICECandidateCalls(func(candidate *webrtc.ICECandidate, target livekit.SignalTarget) error {
		if candidate == nil {
			return nil
		}
		t.Logf("got ICE candidate from B: %v", candidate)
		a.AddICECandidate(candidate.ToJSON())
		return nil
	})
}

func connectTransports(t *testing.T, offerer, answerer *PCTransport, offererHandler, answererHandler *transportfakes.FakeHandler, isICERestart bool, expectedOfferCount int32, expectedAnswerCount int32) {
	var offerCount atomic.Int32
	var answerCount atomic.Int32
	answererHandler.OnAnswerCalls(func(answer webrtc.SessionDescription, answerId uint32, _midToTrackID map[string]string) error {
		answerCount.Inc()
		offerer.HandleRemoteDescription(answer, answerId)
		return nil
	})

	offererHandler.OnOfferCalls(func(offer webrtc.SessionDescription, offerId uint32) error {
		offerCount.Inc()
		answerer.HandleRemoteDescription(offer, offerId)
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

	transportsConnected := untilTransportsConnected(offererHandler, answererHandler)
	transportsConnected.Wait()
}

func untilTransportsConnected(transports ...*transportfakes.FakeHandler) *sync.WaitGroup {
	var triggered sync.WaitGroup
	triggered.Add(len(transports))

	for _, t := range transports {
		var done atomic.Value
		done.Store(false)
		hdlr := func() {
			if val, ok := done.Load().(bool); ok && !val {
				done.Store(true)
				triggered.Done()
			}
		}

		if t.OnInitialConnectedCallCount() != 0 {
			hdlr()
		}
		t.OnInitialConnectedCalls(hdlr)
	}
	return &triggered
}

func TestConfigureAudioTransceiver(t *testing.T) {
	for _, testcase := range []struct {
		nack   bool
		stereo bool
	}{
		{false, false},
		{true, false},
		{false, true},
		{true, true},
	} {
		t.Run(fmt.Sprintf("nack=%v,stereo=%v", testcase.nack, testcase.stereo), func(t *testing.T) {
			var me webrtc.MediaEngine
			registerCodecs(&me, []*livekit.Codec{{Mime: mime.MimeTypeOpus.String()}}, RTCPFeedbackConfig{Audio: []webrtc.RTCPFeedback{{Type: webrtc.TypeRTCPFBNACK}}}, false)
			pc, err := webrtc.NewAPI(webrtc.WithMediaEngine(&me)).NewPeerConnection(webrtc.Configuration{})
			require.NoError(t, err)
			defer pc.Close()
			tr, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly})
			require.NoError(t, err)

			configureSenderAudio(tr, testcase.stereo, testcase.nack)
			codecs := tr.Sender().GetParameters().Codecs
			for _, codec := range codecs {
				if mime.IsMimeTypeStringOpus(codec.MimeType) {
					require.Equal(t, testcase.stereo, strings.Contains(codec.SDPFmtpLine, "sprop-stereo=1"))
					var nackEnabled bool
					for _, fb := range codec.RTCPFeedback {
						if fb.Type == webrtc.TypeRTCPFBNACK {
							nackEnabled = true
							break
						}
					}
					require.Equal(t, testcase.nack, nackEnabled)
				}
			}
		})
	}
}

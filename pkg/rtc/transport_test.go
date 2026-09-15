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

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/rtc/transport"
	"github.com/livekit/livekit-server/pkg/rtc/transport/transportfakes"
	"github.com/livekit/livekit-server/pkg/testutils"
	"github.com/livekit/protocol/codecs/mime"
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
	handlerA.OnOfferCalls(func(sd webrtc.SessionDescription, _offerId uint32, _midToTrackID map[string]string) error {
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
	handlerA.OnOfferCalls(func(sd webrtc.SessionDescription, offerId uint32, _midToTrackID map[string]string) error {
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
	handlerA.OnOfferCalls(func(sd webrtc.SessionDescription, _offerId uint32, _midToTrackID map[string]string) error {
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
	handlerA.OnOfferCalls(func(sd webrtc.SessionDescription, offerId uint32, _midToTrackID map[string]string) error {
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
	handlerA.OnOfferCalls(func(sd webrtc.SessionDescription, offerId uint32, _midToTrackID map[string]string) error {
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
	handlerA.OnOfferCalls(func(sd webrtc.SessionDescription, offerId uint32, _midToTrackID map[string]string) error {
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
	handlerA.OnOfferCalls(func(sd webrtc.SessionDescription, offerId uint32, _midToTrackID map[string]string) error {
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
		EnabledPublishCodecs: []*livekit.Codec{
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

	offererHandler.OnOfferCalls(func(offer webrtc.SessionDescription, offerId uint32, _midToTrackID map[string]string) error {
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

			configureSenderAudio(tr, testcase.stereo, testcase.nack, nil)
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

// When answering a subscriber offer, the sender's audio payload type must echo
// the payload type the offer assigned (RFC 3264), otherwise Firefox decodes no
// audio. See https://github.com/livekit/livekit/issues/4599.
func TestConfigureAudioTransceiverEchoesOfferPayloadType(t *testing.T) {
	var me webrtc.MediaEngine
	registerCodecs(&me, []*livekit.Codec{{Mime: mime.MimeTypeOpus.String()}}, RTCPFeedbackConfig{Audio: []webrtc.RTCPFeedback{{Type: webrtc.TypeRTCPFBNACK}}}, false)
	pc, err := webrtc.NewAPI(webrtc.WithMediaEngine(&me)).NewPeerConnection(webrtc.Configuration{})
	require.NoError(t, err)
	defer pc.Close()
	tr, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly})
	require.NoError(t, err)

	// offer mapped Opus to a payload type different from the server MediaEngine's.
	const offeredOpusPT = webrtc.PayloadType(109)
	configureSenderAudio(tr, false, true, map[mime.MimeType]webrtc.PayloadType{mime.MimeTypeOpus: offeredOpusPT})

	var found bool
	for _, codec := range tr.Sender().GetParameters().Codecs {
		if mime.IsMimeTypeStringOpus(codec.MimeType) {
			require.Equal(t, offeredOpusPT, codec.PayloadType)
			found = true
		}
	}
	require.True(t, found, "opus codec must be present in sender preferences")
}

// In single-PC mode the publisher PC carries both publish and subscribe
// directions. If the MediaEngine were built only from the publish codec list,
// the SDP offer would not advertise some codecs in the m-section even though
// the subscribe direction is supposed to support it. This regression-tests
// the union behavior in newPeerConnection: build the MediaEngine from publish +
// subscribe codec lists.
func TestSinglePCMediaEngineUnionsCodecs(t *testing.T) {
	videoMSectionCodecs := func(transport *PCTransport) []string {
		_, err := transport.pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo)
		require.NoError(t, err)
		offer, err := transport.pc.CreateOffer(nil)
		require.NoError(t, err)
		parsed, err := offer.Unmarshal()
		require.NoError(t, err)
		var rtpmaps []string
		for _, m := range parsed.MediaDescriptions {
			if m.MediaName.Media != "video" {
				continue
			}
			for _, a := range m.Attributes {
				if a.Key == "rtpmap" {
					rtpmaps = append(rtpmaps, a.Value)
				}
			}
		}
		return rtpmaps
	}

	sdpHasH264 := func(rtpmaps []string) bool {
		for _, r := range rtpmaps {
			if strings.Contains(r, "H264/") {
				return true
			}
		}
		return false
	}

	publishOnly := []*livekit.Codec{
		{Mime: mime.MimeTypeOpus.String()},
		{Mime: mime.MimeTypeVP8.String()},
	}
	subscribeOnly := []*livekit.Codec{
		{Mime: mime.MimeTypeOpus.String()},
		{Mime: mime.MimeTypeVP8.String()},
		{Mime: mime.MimeTypeH264.String()},
	}

	// Control: only publish codecs set (dual-PC publisher PC). H.264 absent.
	dualPC, err := NewPCTransport(TransportParams{
		Config:               &WebRTCConfig{},
		EnabledPublishCodecs: publishOnly,
		Handler:              &transportfakes.FakeHandler{},
	})
	require.NoError(t, err)
	require.False(t, sdpHasH264(videoMSectionCodecs(dualPC)),
		"dual-PC publisher must not advertise H.264 when it's stripped from the publish list")

	// Single-PC publisher PC: both lists set. H.264 must appear.
	singlePC, err := NewPCTransport(TransportParams{
		Config:                 &WebRTCConfig{},
		EnabledPublishCodecs:   publishOnly,
		EnabledSubscribeCodecs: subscribeOnly,
		IsSendSide:             true,
		Handler:                &transportfakes.FakeHandler{},
	})
	require.NoError(t, err)
	require.True(t, sdpHasH264(videoMSectionCodecs(singlePC)),
		"single-PC publisher must advertise H.264 from the subscribe list even when it's stripped from the publish list")
}

// Regression test for restrictReceiverCodecsToPublishList: subscribe-only
// codecs (e.g., H.264) registered for subscriptions must not leak into the
// recv-side m-section of an answer, or the peer could publish them.
func TestSinglePCAnswerStripsSubscribeOnlyCodecsFromRecvSide(t *testing.T) {
	publishCodecs := []*livekit.Codec{
		{Mime: mime.MimeTypeOpus.String()},
		{Mime: mime.MimeTypeVP8.String()},
	}
	subscribeCodecs := []*livekit.Codec{
		{Mime: mime.MimeTypeOpus.String()},
		{Mime: mime.MimeTypeVP8.String()},
		{Mime: mime.MimeTypeH264.String()},
	}

	handler := &transportfakes.FakeHandler{}
	server, err := NewPCTransport(TransportParams{
		Config:                 &WebRTCConfig{},
		EnabledPublishCodecs:   publishCodecs,
		EnabledSubscribeCodecs: subscribeCodecs,
		IsSendSide:             true,
		Handler:                handler,
	})
	require.NoError(t, err)
	defer server.Close()

	var clientME webrtc.MediaEngine
	require.NoError(t, registerCodecs(&clientME, subscribeCodecs, RTCPFeedbackConfig{}, false))
	client, err := webrtc.NewAPI(webrtc.WithMediaEngine(&clientME)).NewPeerConnection(webrtc.Configuration{})
	require.NoError(t, err)
	defer client.Close()

	_, err = client.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionSendonly,
	})
	require.NoError(t, err)
	offer, err := client.CreateOffer(nil)
	require.NoError(t, err)
	require.Contains(t, offer.SDP, "H264/", "offer must advertise H.264")
	require.NoError(t, client.SetLocalDescription(offer))

	var answer atomic.Pointer[webrtc.SessionDescription]
	handler.OnAnswerCalls(func(sd webrtc.SessionDescription, _ uint32, _ map[string]string) error {
		answer.Store(&sd)
		return nil
	})
	require.NoError(t, server.HandleRemoteDescription(*client.LocalDescription(), 1))

	require.Eventually(t, func() bool {
		return answer.Load() != nil
	}, 5*time.Second, 10*time.Millisecond, "server did not produce answer")

	parsed, err := answer.Load().Unmarshal()
	require.NoError(t, err)

	var videoSection *sdp.MediaDescription
	for _, m := range parsed.MediaDescriptions {
		if m.MediaName.Media == "video" {
			videoSection = m
			break
		}
	}
	require.NotNil(t, videoSection, "answer missing video m-section")

	for _, a := range videoSection.Attributes {
		if a.Key != "rtpmap" {
			continue
		}
		require.NotContains(t, a.Value, "H264/",
			"answer must not advertise H.264 in recv-side m-section: %s", a.Value)
	}
}

// ---------------------------------------------------------------------------
// inactive media section shrinking

type testOfferSection struct {
	mid      int
	inactive bool
	audio    bool
}

// testOffer builds a Chrome-like client offer with one video section per entry,
// VP8+rtx, header extensions and ssrc lines, bundled with shared ICE credentials.
func testOffer(sections []testOfferSection) string {
	var b strings.Builder
	b.WriteString("v=0\r\no=- 4611731400430051336 2 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\na=group:BUNDLE")
	for _, s := range sections {
		fmt.Fprintf(&b, " %d", s.mid)
	}
	b.WriteString("\r\na=extmap-allow-mixed\r\na=msid-semantic: WMS\r\n")
	for _, s := range sections {
		if s.audio {
			b.WriteString("m=audio 9 UDP/TLS/RTP/SAVPF 111\r\nc=IN IP4 0.0.0.0\r\na=rtcp:9 IN IP4 0.0.0.0\r\n")
		} else {
			b.WriteString("m=video 9 UDP/TLS/RTP/SAVPF 96 97\r\nc=IN IP4 0.0.0.0\r\na=rtcp:9 IN IP4 0.0.0.0\r\n")
		}
		b.WriteString("a=ice-ufrag:AbCdEf01\r\na=ice-pwd:0123456789abcdefghijklmnop\r\na=ice-options:trickle\r\n")
		b.WriteString("a=fingerprint:sha-256 6B:8B:F0:65:5F:78:E2:51:3B:AC:6F:F3:3F:46:1B:35:DC:B8:5F:64:1A:24:C2:43:F0:A1:58:D0:A1:2C:19:08\r\na=setup:actpass\r\n")
		fmt.Fprintf(&b, "a=mid:%d\r\n", s.mid)
		b.WriteString("a=extmap:2 http://www.webrtc.org/experiments/rtp-hdrext/abs-send-time\r\n")
		b.WriteString("a=extmap:4 http://www.ietf.org/id/draft-holmer-rmcat-transport-wide-cc-extensions-01\r\n")
		if s.inactive {
			b.WriteString("a=inactive\r\n")
		} else {
			b.WriteString("a=sendonly\r\n")
		}
		fmt.Fprintf(&b, "a=msid:stream%d track%d\r\na=rtcp-mux\r\na=rtcp-rsize\r\n", s.mid, s.mid)
		if s.audio {
			b.WriteString("a=rtpmap:111 opus/48000/2\r\na=rtcp-fb:111 transport-cc\r\na=fmtp:111 minptime=10;useinbandfec=1\r\n")
		} else {
			b.WriteString("a=rtpmap:96 VP8/90000\r\na=rtcp-fb:96 goog-remb\r\na=rtcp-fb:96 transport-cc\r\na=rtcp-fb:96 nack\r\na=rtcp-fb:96 nack pli\r\n")
			b.WriteString("a=rtpmap:97 rtx/90000\r\na=fmtp:97 apt=96\r\n")
		}
		s1, s2 := 1000000+2*s.mid, 1000001+2*s.mid
		fmt.Fprintf(&b, "a=ssrc-group:FID %d %d\r\n", s1, s2)
		fmt.Fprintf(&b, "a=ssrc:%d cname:c%d\r\na=ssrc:%d msid:stream%d track%d\r\n", s1, s.mid, s1, s.mid, s.mid)
		fmt.Fprintf(&b, "a=ssrc:%d cname:c%d\r\na=ssrc:%d msid:stream%d track%d\r\n", s2, s.mid, s2, s.mid, s.mid)
	}
	return b.String()
}

func TestShrinkInactiveMediaSections(t *testing.T) {
	offer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP: testOffer([]testOfferSection{
			{mid: 0, inactive: true},              // known mid, shrunk
			{mid: 1, inactive: true},              // unknown mid, left alone so pion can create the transceiver
			{mid: 2, inactive: false},             // active, left alone
			{mid: 3, inactive: true, audio: true}, // known mid, audio, only shrunk when included
		}),
	}
	parsed, err := offer.Unmarshal()
	require.NoError(t, err)
	before := make([]int, len(parsed.MediaDescriptions))
	for i, m := range parsed.MediaDescriptions {
		before[i] = len(m.Attributes)
	}
	knownMids := map[string]bool{"0": true, "2": true, "3": true}

	require.Equal(t, 1, shrinkInactiveMediaSections(parsed, knownMids, false))

	shrunk := parsed.MediaDescriptions[0]
	require.Equal(t, []string{"96"}, shrunk.MediaName.Formats)
	for _, a := range shrunk.Attributes {
		require.True(t, inactiveMediaSectionAttributes[a.Key] || a.Key == "rtpmap", "unexpected attribute %s", a.Key)
	}
	rtpmap, ok := shrunk.Attribute("rtpmap")
	require.True(t, ok)
	require.Equal(t, "96 VP8/90000", rtpmap)
	for _, key := range []string{sdp.AttrKeyMID, sdp.AttrKeyInactive, "ice-ufrag", "ice-pwd", "fingerprint", "setup", sdp.AttrKeyRTCPMux} {
		_, ok := shrunk.Attribute(key)
		require.True(t, ok, "missing %s", key)
	}
	require.Less(t, len(shrunk.Attributes), before[0])

	require.Equal(t, before[1], len(parsed.MediaDescriptions[1].Attributes))
	require.Equal(t, before[2], len(parsed.MediaDescriptions[2].Attributes))
	require.Equal(t, before[3], len(parsed.MediaDescriptions[3].Attributes))
	require.Equal(t, []string{"96", "97"}, parsed.MediaDescriptions[1].MediaName.Formats)

	marshalled, err := parsed.Marshal()
	require.NoError(t, err)
	require.Less(t, len(marshalled), len(offer.SDP))

	// audio is shrunk only when included
	parsed, err = offer.Unmarshal()
	require.NoError(t, err)
	require.Equal(t, 2, shrinkInactiveMediaSections(parsed, knownMids, true))
	require.Equal(t, []string{"111"}, parsed.MediaDescriptions[3].MediaName.Formats)
	rtpmap, ok = parsed.MediaDescriptions[3].Attribute("rtpmap")
	require.True(t, ok)
	require.Equal(t, "111 opus/48000/2", rtpmap)
}

// pion must accept a shrunk offer for a transceiver it already has and answer it as before
func TestShrinkInactiveMediaSectionsWithPion(t *testing.T) {
	newTransport := func(shrink bool) *PCTransport {
		tr, err := NewPCTransport(TransportParams{
			Config:               &WebRTCConfig{},
			EnabledPublishCodecs: []*livekit.Codec{{Mime: mime.MimeTypeVP8.String()}},
			Handler:              &transportfakes.FakeHandler{},
			ShrinkInactiveMediaSections: config.ShrinkInactiveMediaSectionsConfig{
				Enabled:    shrink,
				MinSDPSize: 1,
			},
		})
		require.NoError(t, err)
		return tr
	}
	negotiate := func(tr *PCTransport, sections []testOfferSection) string {
		require.NoError(t, tr.setRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: testOffer(sections)}))
		answer, err := tr.pc.CreateAnswer(nil)
		require.NoError(t, err)
		require.NoError(t, tr.pc.SetLocalDescription(answer))
		return answer.SDP
	}
	// only direction and mid lines matter for the answer comparison, ICE candidates differ per run
	answerShape := func(answer string) []string {
		var shape []string
		for _, line := range strings.Split(answer, "\r\n") {
			if strings.HasPrefix(line, "m=") || strings.HasPrefix(line, "a=mid:") || line == "a=inactive" || line == "a=recvonly" || line == "a=sendonly" {
				shape = append(shape, line)
			}
		}
		return shape
	}

	sequence := [][]testOfferSection{
		{{mid: 0}},
		{{mid: 0, inactive: true}, {mid: 1}},
		{{mid: 0, inactive: true}, {mid: 1, inactive: true}, {mid: 2}},
	}

	plain := newTransport(false)
	defer plain.Close()
	shrunk := newTransport(true)
	defer shrunk.Close()
	for i, sections := range sequence {
		plainAnswer := negotiate(plain, sections)
		shrunkAnswer := negotiate(shrunk, sections)
		require.Equal(t, answerShape(plainAnswer), answerShape(shrunkAnswer), "offer %d", i)
	}

	// the shrunk transport gave pion a smaller remote description
	require.Less(t, len(shrunk.pc.CurrentRemoteDescription().SDP), len(plain.pc.CurrentRemoteDescription().SDP))
	require.Equal(t, 3, len(shrunk.pc.GetTransceivers()))
	// pion kept the codecs negotiated when the sections were active
	for _, tr := range shrunk.pc.GetTransceivers() {
		require.NotEmpty(t, tr.Receiver().GetParameters().Codecs, "mid %s", tr.Mid())
	}
}

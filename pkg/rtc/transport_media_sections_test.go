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

package rtc

import (
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/rtc/transport/transportfakes"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/protocol/codecs/mime"
	"github.com/livekit/protocol/livekit"
)

type mediaSectionsHarness struct {
	t       *testing.T
	server  *PCTransport
	handler *transportfakes.FakeHandler
	client  *webrtc.PeerConnection

	mu              sync.Mutex
	answer          *webrtc.SessionDescription
	answers         int
	requirements    int
	requestedAudios uint32
	requestedVideos uint32
	pendingAudios   uint32
	pendingVideos   uint32
	offerID         uint32
}

func newMediaSectionsHarness(t *testing.T) *mediaSectionsHarness {
	codecs := []*livekit.Codec{
		{Mime: mime.MimeTypeOpus.String()},
		{Mime: mime.MimeTypeVP8.String()},
	}

	h := &mediaSectionsHarness{t: t, handler: &transportfakes.FakeHandler{}}

	server, err := NewPCTransport(TransportParams{
		Config:                 &WebRTCConfig{},
		EnabledPublishCodecs:   codecs,
		EnabledSubscribeCodecs: codecs,
		IsSendSide:             true,
		Handler:                h.handler,
	})
	require.NoError(t, err)
	h.server = server

	var clientME webrtc.MediaEngine
	require.NoError(t, registerCodecs(&clientME, codecs, RTCPFeedbackConfig{}, false))
	client, err := webrtc.NewAPI(webrtc.WithMediaEngine(&clientME)).NewPeerConnection(webrtc.Configuration{})
	require.NoError(t, err)
	h.client = client

	h.handler.OnAnswerCalls(func(sd webrtc.SessionDescription, _ uint32, _ map[string]string) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.answer = &sd
		h.answers++
		return nil
	})
	h.handler.OnUnmatchedMediaCalls(func(numAudios uint32, numVideos uint32) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.requirements++
		h.requestedAudios += numAudios
		h.requestedVideos += numVideos
		h.pendingAudios += numAudios
		h.pendingVideos += numVideos
		return nil
	})

	t.Cleanup(func() {
		server.Close()
		client.Close()
	})
	return h
}

func (h *mediaSectionsHarness) negotiate(options *webrtc.OfferOptions) {
	h.mu.Lock()
	for i := uint32(0); i < h.pendingAudios; i++ {
		_, err := h.client.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionRecvonly,
		})
		require.NoError(h.t, err)
	}
	for i := uint32(0); i < h.pendingVideos; i++ {
		_, err := h.client.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionRecvonly,
		})
		require.NoError(h.t, err)
	}
	h.pendingAudios, h.pendingVideos = 0, 0
	answersBefore := h.answers
	h.offerID++
	offerID := h.offerID
	h.mu.Unlock()

	offer, err := h.client.CreateOffer(options)
	require.NoError(h.t, err)
	require.NoError(h.t, h.client.SetLocalDescription(offer))
	require.NoError(h.t, h.server.HandleRemoteDescription(*h.client.LocalDescription(), offerID))

	require.Eventually(h.t, func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.answers > answersBefore
	}, 5*time.Second, 10*time.Millisecond, "server did not produce answer")

	h.mu.Lock()
	answer := *h.answer
	h.mu.Unlock()
	require.NoError(h.t, h.client.SetRemoteDescription(answer))
}

func (h *mediaSectionsHarness) addVideoTrack(id string) (*webrtc.RTPSender, *webrtc.RTPTransceiver) {
	track, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: mime.MimeTypeVP8.String(), ClockRate: 90000},
		id, id,
	)
	require.NoError(h.t, err)

	sender, transceiver, err := h.server.AddTrack(
		track,
		types.AddTrackParams{},
		[]*livekit.Codec{{Mime: mime.MimeTypeVP8.String()}},
		RTCPFeedbackConfig{},
	)
	require.NoError(h.t, err)
	return sender, transceiver
}

func (h *mediaSectionsHarness) requested() (int, uint32, uint32) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.requirements, h.requestedAudios, h.requestedVideos
}

func (h *mediaSectionsHarness) clientVideoTransceivers() int {
	n := 0
	for _, tr := range h.client.GetTransceivers() {
		if tr.Kind() == webrtc.RTPCodecTypeVideo {
			n++
		}
	}
	return n
}

func TestMediaSectionsRequirementNotRepeatedForUnmatchableTransceiver(t *testing.T) {
	h := newMediaSectionsHarness(t)
	_, err := h.client.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionSendonly,
	})
	require.NoError(t, err)
	h.negotiate(nil)
	sender, transceiver := h.addVideoTrack("v1")
	require.Empty(t, transceiver.Mid())
	require.NoError(t, h.server.RemoveTrack(sender))

	const rounds = 5
	for i := 0; i < rounds; i++ {
		h.negotiate(nil)
	}
	h.negotiate(nil)

	_, _, requestedVideos := h.requested()
	require.LessOrEqual(t, requestedVideos, uint32(1),
		"unmatchable transceiver re-requested on every answer: %d video sections requested over %d rounds", requestedVideos, rounds)
	require.LessOrEqual(t, h.clientVideoTransceivers(), 1)
}

func TestMediaSectionsRequirementAfterTransceiverReuse(t *testing.T) {
	h := newMediaSectionsHarness(t)

	_, err := h.client.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionSendonly,
	})
	require.NoError(t, err)
	h.negotiate(nil)

	sender, released := h.addVideoTrack("v1")
	require.NoError(t, h.server.RemoveTrack(sender))
	h.negotiate(nil)
	h.negotiate(nil)
	require.Empty(t, released.Mid())

	_, transceiver := h.addVideoTrack("v2")
	require.Same(t, released, transceiver, "released transceiver should be reused")
	require.Empty(t, transceiver.Mid())
	h.negotiate(nil)
	h.server.Negotiate(true)
	h.negotiate(nil)
	h.negotiate(nil)

	require.NotEmpty(t, transceiver.Mid(), "subscribed track never got a media section")
}

func TestMediaSectionsRequirementReissuedWhenLost(t *testing.T) {
	h := newMediaSectionsHarness(t)

	_, err := h.client.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionSendonly,
	})
	require.NoError(t, err)
	h.negotiate(nil)

	_, transceiver := h.addVideoTrack("v1")
	h.negotiate(nil)
	h.mu.Lock()
	h.pendingVideos = 0
	h.mu.Unlock()

	h.negotiate(&webrtc.OfferOptions{ICERestart: true})
	h.negotiate(nil)
	h.negotiate(nil)

	require.NotEmpty(t, transceiver.Mid(), "lost requirement was never re-issued")
}

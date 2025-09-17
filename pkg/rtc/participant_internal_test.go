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
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"
	"go.uber.org/atomic"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/mime"
	"github.com/livekit/livekit-server/pkg/telemetry/telemetryfakes"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/observability/roomobs"
	lksdp "github.com/livekit/protocol/sdp"
	"github.com/livekit/protocol/signalling"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/protocol/utils/guid"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/routing/routingfakes"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/rtc/types/typesfakes"
	"github.com/livekit/livekit-server/pkg/testutils"
)

func TestIsReady(t *testing.T) {
	tests := []struct {
		state livekit.ParticipantInfo_State
		ready bool
	}{
		{
			state: livekit.ParticipantInfo_JOINING,
			ready: false,
		},
		{
			state: livekit.ParticipantInfo_JOINED,
			ready: true,
		},
		{
			state: livekit.ParticipantInfo_ACTIVE,
			ready: true,
		},
		{
			state: livekit.ParticipantInfo_DISCONNECTED,
			ready: false,
		},
	}

	for _, test := range tests {
		t.Run(test.state.String(), func(t *testing.T) {
			p := &ParticipantImpl{}
			p.state.Store(test.state)
			require.Equal(t, test.ready, p.IsReady())
		})
	}
}

func TestTrackPublishing(t *testing.T) {
	t.Run("should send the correct events", func(t *testing.T) {
		p := newParticipantForTest("test")
		track := &typesfakes.FakeMediaTrack{}
		track.IDReturns("id")
		published := false
		updated := false
		p.OnTrackUpdated(func(p types.LocalParticipant, track types.MediaTrack) {
			updated = true
		})
		p.OnTrackPublished(func(p types.LocalParticipant, track types.MediaTrack) {
			published = true
		})
		p.UpTrackManager.AddPublishedTrack(track)
		p.handleTrackPublished(track, false)
		require.True(t, published)
		require.False(t, updated)
		require.Len(t, p.UpTrackManager.publishedTracks, 1)
	})

	t.Run("sends back trackPublished event", func(t *testing.T) {
		p := newParticipantForTest("test")
		sink := p.params.Sink.(*routingfakes.FakeMessageSink)
		p.AddTrack(&livekit.AddTrackRequest{
			Cid:    "cid",
			Name:   "webcam",
			Type:   livekit.TrackType_VIDEO,
			Width:  1024,
			Height: 768,
		})
		require.Equal(t, 1, sink.WriteMessageCallCount())
		res := sink.WriteMessageArgsForCall(0).(*livekit.SignalResponse)
		require.IsType(t, &livekit.SignalResponse_TrackPublished{}, res.Message)
		published := res.Message.(*livekit.SignalResponse_TrackPublished).TrackPublished
		require.Equal(t, "cid", published.Cid)
		require.Equal(t, "webcam", published.Track.Name)
		require.Equal(t, livekit.TrackType_VIDEO, published.Track.Type)
		require.Equal(t, uint32(1024), published.Track.Width)
		require.Equal(t, uint32(768), published.Track.Height)
	})

	t.Run("should not allow adding of duplicate tracks", func(t *testing.T) {
		p := newParticipantForTest("test")
		sink := p.params.Sink.(*routingfakes.FakeMessageSink)
		p.AddTrack(&livekit.AddTrackRequest{
			Cid:  "cid",
			Name: "webcam",
			Type: livekit.TrackType_VIDEO,
		})
		p.AddTrack(&livekit.AddTrackRequest{
			Cid:  "cid",
			Name: "duplicate",
			Type: livekit.TrackType_AUDIO,
		})

		// error response on duplicate adds a message
		require.Equal(t, 2, sink.WriteMessageCallCount())
	})

	t.Run("should queue adding of duplicate tracks if already published by client id in signalling", func(t *testing.T) {
		p := newParticipantForTest("test")
		sink := p.params.Sink.(*routingfakes.FakeMessageSink)

		track := &typesfakes.FakeLocalMediaTrack{}
		track.HasSignalCidCalls(func(s string) bool { return s == "cid" })
		track.ToProtoReturns(&livekit.TrackInfo{})
		// directly add to publishedTracks without lock - for testing purpose only
		p.UpTrackManager.publishedTracks["cid"] = track

		p.AddTrack(&livekit.AddTrackRequest{
			Cid:  "cid",
			Name: "webcam",
			Type: livekit.TrackType_VIDEO,
		})
		// `queued` `RequestResponse` should add a message
		require.Equal(t, 1, sink.WriteMessageCallCount())
		require.Equal(t, 1, len(p.pendingTracks["cid"].trackInfos))

		// add again - it should be added to the queue
		p.AddTrack(&livekit.AddTrackRequest{
			Cid:  "cid",
			Name: "webcam",
			Type: livekit.TrackType_VIDEO,
		})
		// `queued` `RequestResponse`s should have been sent for duplicate additions
		require.Equal(t, 2, sink.WriteMessageCallCount())
		require.Equal(t, 2, len(p.pendingTracks["cid"].trackInfos))

		// check SID is the same
		require.Equal(t, p.pendingTracks["cid"].trackInfos[0].Sid, p.pendingTracks["cid"].trackInfos[1].Sid)
	})

	t.Run("should queue adding of duplicate tracks if already published by client id in sdp", func(t *testing.T) {
		p := newParticipantForTest("test")
		sink := p.params.Sink.(*routingfakes.FakeMessageSink)

		track := &typesfakes.FakeLocalMediaTrack{}
		track.ToProtoReturns(&livekit.TrackInfo{})
		track.HasSdpCidCalls(func(s string) bool { return s == "cid" })
		// directly add to publishedTracks without lock - for testing purpose only
		p.UpTrackManager.publishedTracks["cid"] = track

		p.AddTrack(&livekit.AddTrackRequest{
			Cid:  "cid",
			Name: "webcam",
			Type: livekit.TrackType_VIDEO,
		})
		// `queued` `RequestResponse` should add a message
		require.Equal(t, 1, sink.WriteMessageCallCount())
		require.Equal(t, 1, len(p.pendingTracks["cid"].trackInfos))

		// add again - it should be added to the queue
		p.AddTrack(&livekit.AddTrackRequest{
			Cid:  "cid",
			Name: "webcam",
			Type: livekit.TrackType_VIDEO,
		})
		// `queued` `RequestResponse`s should have been sent for duplicate additions
		require.Equal(t, 2, sink.WriteMessageCallCount())
		require.Equal(t, 2, len(p.pendingTracks["cid"].trackInfos))

		// check SID is the same
		require.Equal(t, p.pendingTracks["cid"].trackInfos[0].Sid, p.pendingTracks["cid"].trackInfos[1].Sid)
	})

	t.Run("should not allow adding disallowed sources", func(t *testing.T) {
		p := newParticipantForTest("test")
		p.SetPermission(&livekit.ParticipantPermission{
			CanPublish: true,
			CanPublishSources: []livekit.TrackSource{
				livekit.TrackSource_CAMERA,
			},
		})
		sink := p.params.Sink.(*routingfakes.FakeMessageSink)
		p.AddTrack(&livekit.AddTrackRequest{
			Cid:    "cid",
			Name:   "webcam",
			Source: livekit.TrackSource_CAMERA,
			Type:   livekit.TrackType_VIDEO,
		})
		require.Equal(t, 1, sink.WriteMessageCallCount())

		p.AddTrack(&livekit.AddTrackRequest{
			Cid:    "cid2",
			Name:   "rejected source",
			Type:   livekit.TrackType_AUDIO,
			Source: livekit.TrackSource_MICROPHONE,
		})
		// an error response for disallowed source should send a `RequestResponse`.
		require.Equal(t, 2, sink.WriteMessageCallCount())
	})
}

func TestOutOfOrderUpdates(t *testing.T) {
	p := newParticipantForTest("test")
	p.updateState(livekit.ParticipantInfo_JOINED)
	p.SetMetadata("initial metadata")
	sink := p.GetResponseSink().(*routingfakes.FakeMessageSink)
	pi1 := p.ToProto()
	p.SetMetadata("second update")
	pi2 := p.ToProto()

	require.Greater(t, pi2.Version, pi1.Version)

	// send the second update first
	require.NoError(t, p.SendParticipantUpdate([]*livekit.ParticipantInfo{pi2}))
	require.NoError(t, p.SendParticipantUpdate([]*livekit.ParticipantInfo{pi1}))

	// only sent once, and it's the earlier message
	require.Equal(t, 1, sink.WriteMessageCallCount())
	sent := sink.WriteMessageArgsForCall(0).(*livekit.SignalResponse)
	require.Equal(t, "second update", sent.GetUpdate().Participants[0].Metadata)
}

// after disconnection, things should continue to function and not panic
func TestDisconnectTiming(t *testing.T) {
	t.Run("Negotiate doesn't panic after channel closed", func(t *testing.T) {
		p := newParticipantForTest("test")
		msg := routing.NewMessageChannel(livekit.ConnectionID("test"), routing.DefaultMessageChannelSize)
		p.params.Sink = msg
		go func() {
			for msg := range msg.ReadChan() {
				t.Log("received message from chan", msg)
			}
		}()
		track := &typesfakes.FakeMediaTrack{}
		p.UpTrackManager.AddPublishedTrack(track)
		p.handleTrackPublished(track, false)

		// close channel and then try to Negotiate
		msg.Close()
	})
}

func TestCorrectJoinedAt(t *testing.T) {
	p := newParticipantForTest("test")
	info := p.ToProto()
	require.NotZero(t, info.JoinedAt)
	require.True(t, time.Now().Unix()-info.JoinedAt <= 1)
}

func TestMuteSetting(t *testing.T) {
	t.Run("can set mute when track is pending", func(t *testing.T) {
		p := newParticipantForTest("test")
		ti := &livekit.TrackInfo{Sid: "testTrack"}
		p.pendingTracks["cid"] = &pendingTrackInfo{trackInfos: []*livekit.TrackInfo{ti}}

		p.SetTrackMuted(&livekit.MuteTrackRequest{
			Sid:   ti.Sid,
			Muted: true,
		}, false)
		require.True(t, p.pendingTracks["cid"].trackInfos[0].Muted)
	})

	t.Run("can publish a muted track", func(t *testing.T) {
		p := newParticipantForTest("test")
		p.AddTrack(&livekit.AddTrackRequest{
			Cid:   "cid",
			Type:  livekit.TrackType_AUDIO,
			Muted: true,
		})

		_, ti, _, _, _ := p.getPendingTrack("cid", livekit.TrackType_AUDIO, false)
		require.NotNil(t, ti)
		require.True(t, ti.Muted)
	})
}

func TestSubscriberAsPrimary(t *testing.T) {
	t.Run("protocol 4 uses subs as primary", func(t *testing.T) {
		p := newParticipantForTestWithOpts("test", &participantOpts{
			permissions: &livekit.ParticipantPermission{
				CanSubscribe: true,
				CanPublish:   true,
			},
		})
		require.True(t, p.SubscriberAsPrimary())
	})

	t.Run("protocol 2 uses pub as primary", func(t *testing.T) {
		p := newParticipantForTestWithOpts("test", &participantOpts{
			protocolVersion: 2,
			permissions: &livekit.ParticipantPermission{
				CanSubscribe: true,
				CanPublish:   true,
			},
		})
		require.False(t, p.SubscriberAsPrimary())
	})

	t.Run("publisher only uses pub as primary", func(t *testing.T) {
		p := newParticipantForTestWithOpts("test", &participantOpts{
			permissions: &livekit.ParticipantPermission{
				CanSubscribe: false,
				CanPublish:   true,
			},
		})
		require.False(t, p.SubscriberAsPrimary())

		// ensure that it doesn't change after perms
		p.SetPermission(&livekit.ParticipantPermission{
			CanSubscribe: true,
			CanPublish:   true,
		})
		require.False(t, p.SubscriberAsPrimary())
	})
}

func TestDisableCodecs(t *testing.T) {
	participant := newParticipantForTestWithOpts("123", &participantOpts{
		publisher: false,
		clientConf: &livekit.ClientConfiguration{
			DisabledCodecs: &livekit.DisabledCodecs{
				Codecs: []*livekit.Codec{
					{Mime: "video/h264"},
				},
			},
		},
	})

	participant.SetMigrateState(types.MigrateStateComplete)

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	require.NoError(t, err)
	transceiver, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendrecv})
	require.NoError(t, err)
	sdp, err := pc.CreateOffer(nil)
	require.NoError(t, err)
	pc.SetLocalDescription(sdp)
	codecs := transceiver.Receiver().GetParameters().Codecs
	var found264 bool
	for _, c := range codecs {
		if mime.IsMimeTypeStringH264(c.MimeType) {
			found264 = true
		}
	}
	require.True(t, found264)
	offerId := uint32(42)

	// negotiated codec should not contain h264
	sink := &routingfakes.FakeMessageSink{}
	participant.SetResponseSink(sink)
	var answer webrtc.SessionDescription
	var answerId uint32
	var answerReceived atomic.Bool
	var answerIdReceived atomic.Uint32
	sink.WriteMessageCalls(func(msg proto.Message) error {
		if res, ok := msg.(*livekit.SignalResponse); ok {
			if res.GetAnswer() != nil {
				answer, answerId = signalling.FromProtoSessionDescription(res.GetAnswer())
				answerReceived.Store(true)
				answerIdReceived.Store(answerId)
			}
		}
		return nil
	})
	participant.HandleOffer(&livekit.SessionDescription{
		Type: webrtc.SDPTypeOffer.String(),
		Sdp:  sdp.SDP,
		Id:   offerId,
	})

	testutils.WithTimeout(t, func() string {
		if answerReceived.Load() && answerIdReceived.Load() == offerId {
			return ""
		} else {
			return "answer not received OR answer id mismatch"
		}
	})
	require.NoError(t, pc.SetRemoteDescription(answer), answer.SDP, sdp.SDP)

	codecs = transceiver.Receiver().GetParameters().Codecs
	found264 = false
	for _, c := range codecs {
		if mime.IsMimeTypeStringH264(c.MimeType) {
			found264 = true
		}
	}
	require.False(t, found264)
}

func TestDisablePublishCodec(t *testing.T) {
	participant := newParticipantForTestWithOpts("123", &participantOpts{
		publisher: true,
		clientConf: &livekit.ClientConfiguration{
			DisabledCodecs: &livekit.DisabledCodecs{
				Publish: []*livekit.Codec{
					{Mime: "video/h264"},
				},
			},
		},
	})

	for _, codec := range participant.enabledPublishCodecs {
		require.False(t, mime.IsMimeTypeStringH264(codec.Mime))
	}

	sink := &routingfakes.FakeMessageSink{}
	participant.SetResponseSink(sink)
	var publishReceived atomic.Bool
	sink.WriteMessageCalls(func(msg proto.Message) error {
		if res, ok := msg.(*livekit.SignalResponse); ok {
			if published := res.GetTrackPublished(); published != nil {
				publishReceived.Store(true)
				require.NotEmpty(t, published.Track.Codecs)
				require.True(t, mime.IsMimeTypeStringVP8(published.Track.Codecs[0].MimeType))
			}
		}
		return nil
	})

	// simulcast codec response should pick an alternative
	participant.AddTrack(&livekit.AddTrackRequest{
		Cid:  "cid1",
		Type: livekit.TrackType_VIDEO,
		SimulcastCodecs: []*livekit.SimulcastCodec{{
			Codec: "h264",
			Cid:   "cid1",
		}},
	})

	require.Eventually(t, func() bool { return publishReceived.Load() }, 5*time.Second, 10*time.Millisecond)

	// publishing a supported codec should not change
	publishReceived.Store(false)
	sink.WriteMessageCalls(func(msg proto.Message) error {
		if res, ok := msg.(*livekit.SignalResponse); ok {
			if published := res.GetTrackPublished(); published != nil {
				publishReceived.Store(true)
				require.NotEmpty(t, published.Track.Codecs)
				require.True(t, mime.IsMimeTypeStringVP8(published.Track.Codecs[0].MimeType))
			}
		}
		return nil
	})
	participant.AddTrack(&livekit.AddTrackRequest{
		Cid:  "cid2",
		Type: livekit.TrackType_VIDEO,
		SimulcastCodecs: []*livekit.SimulcastCodec{{
			Codec: "vp8",
			Cid:   "cid2",
		}},
	})
	require.Eventually(t, func() bool { return publishReceived.Load() }, 5*time.Second, 10*time.Millisecond)
}

func TestPreferMediaCodecForPublisher(t *testing.T) {
	testCases := []struct {
		name                       string
		mediaKind                  string
		trackBaseCid               string
		preferredCodec             string
		addTrack                   *livekit.AddTrackRequest
		mimeTypeStringChecker      func(string) bool
		mimeTypeCodecStringChecker func(string) bool
		transceiverMimeType        mime.MimeType
	}{
		{
			name:           "video",
			mediaKind:      "video",
			trackBaseCid:   "preferH264Video",
			preferredCodec: "h264",
			addTrack: &livekit.AddTrackRequest{
				Type:   livekit.TrackType_VIDEO,
				Name:   "video",
				Width:  1280,
				Height: 720,
				Source: livekit.TrackSource_CAMERA,
			},
			mimeTypeStringChecker:      mime.IsMimeTypeStringH264,
			mimeTypeCodecStringChecker: mime.IsMimeTypeCodecStringH264,
			transceiverMimeType:        mime.MimeTypeVP8,
		},
		{
			name:           "audio",
			mediaKind:      "audio",
			trackBaseCid:   "preferPCMAAudio",
			preferredCodec: "pcma",
			addTrack: &livekit.AddTrackRequest{
				Type:   livekit.TrackType_AUDIO,
				Name:   "audio",
				Source: livekit.TrackSource_MICROPHONE,
			},
			mimeTypeStringChecker:      mime.IsMimeTypeStringPCMA,
			mimeTypeCodecStringChecker: mime.IsMimeTypeCodecStringPCMA,
			transceiverMimeType:        mime.MimeTypeOpus,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			participant := newParticipantForTestWithOpts("123", &participantOpts{
				publisher: true,
			})
			participant.SetMigrateState(types.MigrateStateComplete)

			pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
			require.NoError(t, err)
			defer pc.Close()

			for i := 0; i < 2; i++ {
				// publish preferred track without client using setCodecPreferences()
				trackCid := fmt.Sprintf("%s-%d", tc.trackBaseCid, i)
				req := utils.CloneProto(tc.addTrack)
				req.SimulcastCodecs = []*livekit.SimulcastCodec{
					{
						Codec: tc.preferredCodec,
						Cid:   trackCid,
					},
				}
				participant.AddTrack(req)

				track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: tc.transceiverMimeType.String()}, trackCid, trackCid)
				require.NoError(t, err)
				transceiver, err := pc.AddTransceiverFromTrack(track, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendrecv})
				require.NoError(t, err)
				codecs := transceiver.Receiver().GetParameters().Codecs

				if i > 0 {
					// the negotiated codecs order could be updated by first negotiation,
					// reorder to make tested preferred codec not preferred
					for tc.mimeTypeStringChecker(codecs[0].MimeType) {
						codecs = append(codecs[1:], codecs[0])
					}
				}
				// preferred codec should not be preferred in `offer`
				require.False(t, tc.mimeTypeStringChecker(codecs[0].MimeType), "codecs", codecs)

				sdp, err := pc.CreateOffer(nil)
				require.NoError(t, err)
				require.NoError(t, pc.SetLocalDescription(sdp))
				offerId := uint32(23)

				sink := &routingfakes.FakeMessageSink{}
				participant.SetResponseSink(sink)
				var answer webrtc.SessionDescription
				var answerId uint32
				var answerReceived atomic.Bool
				var answerIdReceived atomic.Uint32
				sink.WriteMessageCalls(func(msg proto.Message) error {
					if res, ok := msg.(*livekit.SignalResponse); ok {
						if res.GetAnswer() != nil {
							answer, answerId = signalling.FromProtoSessionDescription(res.GetAnswer())
							pc.SetRemoteDescription(answer)
							answerReceived.Store(true)
							answerIdReceived.Store(answerId)
						}
					}
					return nil
				})
				participant.HandleOffer(&livekit.SessionDescription{
					Type: webrtc.SDPTypeOffer.String(),
					Sdp:  sdp.SDP,
					Id:   offerId,
				})

				require.Eventually(t, func() bool { return answerReceived.Load() && answerIdReceived.Load() == offerId }, 5*time.Second, 10*time.Millisecond)

				var havePreferred bool
				parsed, err := answer.Unmarshal()
				require.NoError(t, err)
				var mediaSectionIndex int
				for _, m := range parsed.MediaDescriptions {
					if m.MediaName.Media == tc.mediaKind {
						if mediaSectionIndex == i {
							codecs, err := lksdp.CodecsFromMediaDescription(m)
							require.NoError(t, err)
							if tc.mimeTypeCodecStringChecker(codecs[0].Name) {
								havePreferred = true
								break
							}
						}
						mediaSectionIndex++
					}
				}

				require.Truef(t, havePreferred, "%s should be preferred for %s section %d, answer sdp: \n%s", tc.preferredCodec, tc.mediaKind, i, answer.SDP)
			}
		})
	}
}

func TestPreferAudioCodecForRed(t *testing.T) {
	participant := newParticipantForTestWithOpts("123", &participantOpts{
		publisher: true,
	})
	participant.SetMigrateState(types.MigrateStateComplete)

	me := webrtc.MediaEngine{}
	opusCodecParameters := OpusCodecParameters
	opusCodecParameters.RTPCodecCapability.RTCPFeedback = []webrtc.RTCPFeedback{{Type: webrtc.TypeRTCPFBNACK}}
	require.NoError(t, me.RegisterCodec(opusCodecParameters, webrtc.RTPCodecTypeAudio))
	redCodecParameters := RedCodecParameters
	redCodecParameters.RTPCodecCapability.RTCPFeedback = []webrtc.RTCPFeedback{{Type: webrtc.TypeRTCPFBNACK}}
	require.NoError(t, me.RegisterCodec(redCodecParameters, webrtc.RTPCodecTypeAudio))

	api := webrtc.NewAPI(webrtc.WithMediaEngine(&me))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	require.NoError(t, err)
	defer pc.Close()

	for idx, disableRed := range []bool{false, true, false, true} {
		t.Run(fmt.Sprintf("disableRed=%v", disableRed), func(t *testing.T) {
			trackCid := fmt.Sprintf("audiotrack%d", idx)
			req := &livekit.AddTrackRequest{
				Type: livekit.TrackType_AUDIO,
				Cid:  trackCid,
			}
			if idx < 2 {
				req.DisableRed = disableRed
			} else {
				codec := "red"
				if disableRed {
					codec = "opus"
				}
				req.SimulcastCodecs = []*livekit.SimulcastCodec{
					{
						Codec: codec,
						Cid:   trackCid,
					},
				}
			}
			participant.AddTrack(req)

			track, err := webrtc.NewTrackLocalStaticRTP(
				webrtc.RTPCodecCapability{MimeType: "audio/opus"},
				trackCid,
				trackCid,
			)
			require.NoError(t, err)

			transceiver, err := pc.AddTransceiverFromTrack(
				track,
				webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendrecv},
			)
			require.NoError(t, err)
			codecs := transceiver.Sender().GetParameters().Codecs
			for i, c := range codecs {
				if c.MimeType == "audio/opus" {
					if i != 0 {
						codecs[0], codecs[i] = codecs[i], codecs[0]
					}
					break
				}
			}
			transceiver.SetCodecPreferences(codecs)
			sdp, err := pc.CreateOffer(nil)
			require.NoError(t, err)
			pc.SetLocalDescription(sdp)
			// opus should be preferred
			require.Equal(t, codecs[0].MimeType, "audio/opus", sdp)
			offerId := uint32(0xffffff)

			sink := &routingfakes.FakeMessageSink{}
			participant.SetResponseSink(sink)
			var answer webrtc.SessionDescription
			var answerId uint32
			var answerReceived atomic.Bool
			var answerIdReceived atomic.Uint32
			sink.WriteMessageCalls(func(msg proto.Message) error {
				if res, ok := msg.(*livekit.SignalResponse); ok {
					if res.GetAnswer() != nil {
						answer, answerId = signalling.FromProtoSessionDescription(res.GetAnswer())
						pc.SetRemoteDescription(answer)
						answerReceived.Store(true)
						answerIdReceived.Store(answerId)
					}
				}
				return nil
			})
			participant.HandleOffer(&livekit.SessionDescription{
				Type: webrtc.SDPTypeOffer.String(),
				Sdp:  sdp.SDP,
				Id:   offerId,
			})
			require.Eventually(
				t,
				func() bool {
					return answerReceived.Load() && answerIdReceived.Load() == offerId
				},
				5*time.Second,
				10*time.Millisecond,
			)

			var redPreferred bool
			parsed, err := answer.Unmarshal()
			require.NoError(t, err)
			var audioSectionIndex int
			for _, m := range parsed.MediaDescriptions {
				if m.MediaName.Media == "audio" {
					if audioSectionIndex == idx {
						codecs, err := lksdp.CodecsFromMediaDescription(m)
						require.NoError(t, err)
						// nack is always enabled. if red is preferred, server will not generate nack request
						var nackEnabled bool
						for _, c := range codecs {
							if c.Name == "opus" {
								for _, fb := range c.RTCPFeedback {
									if strings.Contains(fb, "nack") {
										nackEnabled = true
										break
									}
								}
							}
						}
						require.True(t, nackEnabled, "nack should be enabled for opus")

						if mime.IsMimeTypeCodecStringRED(codecs[0].Name) {
							redPreferred = true
							break
						}
					}
					audioSectionIndex++
				}
			}
			require.Equalf(t, !disableRed, redPreferred, "offer : \n%s\nanswer sdp: \n%s", sdp, answer.SDP)
		})
	}
}

type participantOpts struct {
	permissions     *livekit.ParticipantPermission
	protocolVersion types.ProtocolVersion
	publisher       bool
	clientConf      *livekit.ClientConfiguration
	clientInfo      *livekit.ClientInfo
}

func newParticipantForTestWithOpts(identity livekit.ParticipantIdentity, opts *participantOpts) *ParticipantImpl {
	if opts == nil {
		opts = &participantOpts{}
	}
	if opts.protocolVersion == 0 {
		opts.protocolVersion = 6
	}
	conf, _ := config.NewConfig("", true, nil, nil)
	// disable mux, it doesn't play too well with unit test
	conf.RTC.TCPPort = 0
	rtcConf, err := NewWebRTCConfig(conf)
	if err != nil {
		panic(err)
	}
	ff := buffer.NewFactoryOfBufferFactory(500, 200)
	rtcConf.SetBufferFactory(ff.CreateBufferFactory())
	grants := &auth.ClaimGrants{
		Video: &auth.VideoGrant{},
	}
	if opts.permissions != nil {
		grants.Video.SetCanPublish(opts.permissions.CanPublish)
		grants.Video.SetCanPublishData(opts.permissions.CanPublishData)
		grants.Video.SetCanSubscribe(opts.permissions.CanSubscribe)
	}

	enabledCodecs := make([]*livekit.Codec, 0, len(conf.Room.EnabledCodecs))
	for _, c := range conf.Room.EnabledCodecs {
		enabledCodecs = append(enabledCodecs, &livekit.Codec{
			Mime:     c.Mime,
			FmtpLine: c.FmtpLine,
		})
	}
	sid := livekit.ParticipantID(guid.New(utils.ParticipantPrefix))
	p, _ := NewParticipant(ParticipantParams{
		SID:                    sid,
		Identity:               identity,
		Config:                 rtcConf,
		Sink:                   &routingfakes.FakeMessageSink{},
		ProtocolVersion:        opts.protocolVersion,
		SessionStartTime:       time.Now(),
		PLIThrottleConfig:      conf.RTC.PLIThrottle,
		Grants:                 grants,
		PublishEnabledCodecs:   enabledCodecs,
		SubscribeEnabledCodecs: enabledCodecs,
		ClientConf:             opts.clientConf,
		ClientInfo:             ClientInfo{ClientInfo: opts.clientInfo},
		Logger:                 LoggerWithParticipant(logger.GetLogger(), identity, sid, false),
		Reporter:               roomobs.NewNoopParticipantSessionReporter(),
		Telemetry:              &telemetryfakes.FakeTelemetryService{},
		VersionGenerator:       utils.NewDefaultTimedVersionGenerator(),
		ParticipantHelper:      &typesfakes.FakeLocalParticipantHelper{},
	})
	p.isPublisher.Store(opts.publisher)
	p.updateState(livekit.ParticipantInfo_ACTIVE)

	return p
}

func newParticipantForTest(identity livekit.ParticipantIdentity) *ParticipantImpl {
	return newParticipantForTestWithOpts(identity, nil)
}

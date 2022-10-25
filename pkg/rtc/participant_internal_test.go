package rtc

import (
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v3"
	"github.com/stretchr/testify/require"
	"go.uber.org/atomic"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/utils"

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
		p.state.Store(livekit.ParticipantInfo_ACTIVE)
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
		p.handleTrackPublished(track)

		require.True(t, published)
		require.False(t, updated)
		require.Len(t, p.UpTrackManager.publishedTracks, 1)

		track.AddOnCloseArgsForCall(0)()
		require.Len(t, p.UpTrackManager.publishedTracks, 0)
		require.True(t, updated)
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

		require.Equal(t, 1, sink.WriteMessageCallCount())
	})

	t.Run("should queue adding of duplicate tracks if already published by client id in signalling", func(t *testing.T) {
		p := newParticipantForTest("test")
		sink := p.params.Sink.(*routingfakes.FakeMessageSink)

		track := &typesfakes.FakeLocalMediaTrack{}
		track.SignalCidReturns("cid")
		track.ToProtoReturns(&livekit.TrackInfo{})
		// directly add to publishedTracks without lock - for testing purpose only
		p.UpTrackManager.publishedTracks["cid"] = track

		p.AddTrack(&livekit.AddTrackRequest{
			Cid:  "cid",
			Name: "webcam",
			Type: livekit.TrackType_VIDEO,
		})
		require.Equal(t, 0, sink.WriteMessageCallCount())
		require.Equal(t, 1, len(p.pendingTracks["cid"].trackInfos))

		// add again - it should be added to the queue
		p.AddTrack(&livekit.AddTrackRequest{
			Cid:  "cid",
			Name: "webcam",
			Type: livekit.TrackType_VIDEO,
		})
		require.Equal(t, 0, sink.WriteMessageCallCount())
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
		require.Equal(t, 0, sink.WriteMessageCallCount())
		require.Equal(t, 1, len(p.pendingTracks["cid"].trackInfos))

		// add again - it should be added to the queue
		p.AddTrack(&livekit.AddTrackRequest{
			Cid:  "cid",
			Name: "webcam",
			Type: livekit.TrackType_VIDEO,
		})
		require.Equal(t, 0, sink.WriteMessageCallCount())
		require.Equal(t, 2, len(p.pendingTracks["cid"].trackInfos))

		// check SID is the same
		require.Equal(t, p.pendingTracks["cid"].trackInfos[0].Sid, p.pendingTracks["cid"].trackInfos[1].Sid)
	})
}

func TestOutOfOrderUpdates(t *testing.T) {
	p := newParticipantForTest("test")
	p.SetMetadata("initial metadata")
	sink := p.getResponseSink().(*routingfakes.FakeMessageSink)
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
		msg := routing.NewMessageChannel(routing.DefaultMessageChannelSize)
		p.params.Sink = msg
		go func() {
			for msg := range msg.ReadChan() {
				t.Log("received message from chan", msg)
			}
		}()
		track := &typesfakes.FakeMediaTrack{}
		p.UpTrackManager.AddPublishedTrack(track)
		p.handleTrackPublished(track)

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

		p.SetTrackMuted(livekit.TrackID(ti.Sid), true, false)
		require.True(t, ti.Muted)
	})

	t.Run("can publish a muted track", func(t *testing.T) {
		p := newParticipantForTest("test")
		p.AddTrack(&livekit.AddTrackRequest{
			Cid:   "cid",
			Type:  livekit.TrackType_AUDIO,
			Muted: true,
		})

		_, ti := p.getPendingTrack("cid", livekit.TrackType_AUDIO)
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

func TestSetStableTrackID(t *testing.T) {
	testCases := []struct {
		name                 string
		trackInfo            *livekit.TrackInfo
		unpublished          []*livekit.TrackInfo
		cid                  string
		prefix               string
		remainingUnpublished int
	}{
		{
			name: "first track, generates new ID",
			trackInfo: &livekit.TrackInfo{
				Type:   livekit.TrackType_VIDEO,
				Source: livekit.TrackSource_CAMERA,
			},
			prefix: "TR_VC",
		},
		{
			name: "re-using existing ID",
			trackInfo: &livekit.TrackInfo{
				Type:   livekit.TrackType_VIDEO,
				Source: livekit.TrackSource_CAMERA,
			},
			unpublished: []*livekit.TrackInfo{
				{
					Type:   livekit.TrackType_VIDEO,
					Source: livekit.TrackSource_SCREEN_SHARE,
					Sid:    "TR_VC1234",
				},
				{
					Type:   livekit.TrackType_VIDEO,
					Source: livekit.TrackSource_CAMERA,
					Sid:    "TR_VC1235",
				},
			},
			cid:                  "TR_VC1235",
			prefix:               "TR_VC1235",
			remainingUnpublished: 1,
		},
		{
			name: "mismatch name for reuse",
			trackInfo: &livekit.TrackInfo{
				Type:   livekit.TrackType_VIDEO,
				Source: livekit.TrackSource_CAMERA,
				Name:   "new_name",
			},
			unpublished: []*livekit.TrackInfo{
				{
					Type:   livekit.TrackType_VIDEO,
					Source: livekit.TrackSource_CAMERA,
					Sid:    "TR_NotUsed",
				},
			},
			prefix:               "TR_VC",
			remainingUnpublished: 1,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			p := newParticipantForTest("test")
			p.unpublishedTracks = tc.unpublished

			ti := tc.trackInfo
			p.setStableTrackID(tc.cid, ti)
			require.Contains(t, ti.Sid, tc.prefix)
			require.Len(t, p.unpublishedTracks, tc.remainingUnpublished)
		})
	}
}

func TestDisableCodecs(t *testing.T) {
	participant := newParticipantForTestWithOpts(livekit.ParticipantIdentity("123"), &participantOpts{
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
		if strings.EqualFold(c.MimeType, "video/h264") {
			found264 = true
		}
	}
	require.True(t, found264)

	// negotiated codec should not contain h264
	sink := &routingfakes.FakeMessageSink{}
	participant.SetResponseSink(sink)
	var answer webrtc.SessionDescription
	var answerReceived atomic.Bool
	sink.WriteMessageStub = func(msg proto.Message) error {
		if res, ok := msg.(*livekit.SignalResponse); ok {
			if res.GetAnswer() != nil {
				answer = FromProtoSessionDescription(res.GetAnswer())
				answerReceived.Store(true)
			}
		}
		return nil
	}
	participant.HandleOffer(sdp)

	testutils.WithTimeout(t, func() string {
		if answerReceived.Load() {
			return ""
		} else {
			return "answer not received"
		}
	})
	require.NoError(t, pc.SetRemoteDescription(answer), answer.SDP, sdp.SDP)

	codecs = transceiver.Receiver().GetParameters().Codecs
	found264 = false
	for _, c := range codecs {
		if strings.EqualFold(c.MimeType, "video/h264") {
			found264 = true
		}
	}
	require.False(t, found264)
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
	conf.RTC.UDPPort = 0
	conf.RTC.TCPPort = 0
	rtcConf, err := NewWebRTCConfig(conf, "")
	if err != nil {
		panic(err)
	}
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
	p, _ := NewParticipant(ParticipantParams{
		SID:               livekit.ParticipantID(utils.NewGuid(utils.ParticipantPrefix)),
		Identity:          identity,
		Config:            rtcConf,
		Sink:              &routingfakes.FakeMessageSink{},
		ProtocolVersion:   opts.protocolVersion,
		PLIThrottleConfig: conf.RTC.PLIThrottle,
		Grants:            grants,
		EnabledCodecs:     enabledCodecs,
		ClientConf:        opts.clientConf,
		ClientInfo:        ClientInfo{ClientInfo: opts.clientInfo},
	})
	p.isPublisher.Store(opts.publisher)

	return p
}

func newParticipantForTest(identity livekit.ParticipantIdentity) *ParticipantImpl {
	return newParticipantForTestWithOpts(identity, nil)
}

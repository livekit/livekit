package rtc

import (
	"testing"
	"time"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/pion/webrtc/v3"
	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/routing/routingfakes"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/rtc/types/typesfakes"
	"github.com/livekit/livekit-server/pkg/sfu/connectionquality"
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

func TestICEStateChange(t *testing.T) {
	t.Run("onClose gets called when ICE disconnected", func(t *testing.T) {
		p := newParticipantForTest("test")
		closeChan := make(chan struct{})
		p.onClose = func(participant types.LocalParticipant, disallowedSubscriptions map[livekit.TrackID]livekit.ParticipantID) {
			close(closeChan)
		}
		p.handlePrimaryStateChange(webrtc.PeerConnectionStateFailed)

		select {
		case <-closeChan:
			return
		case <-time.After(time.Millisecond * 10):
			t.Fatalf("onClose was not called after timeout")
		}
	})
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

	t.Run("should not allow adding of duplicate tracks if already published by client id in signalling", func(t *testing.T) {
		p := newParticipantForTest("test")
		sink := p.params.Sink.(*routingfakes.FakeMessageSink)

		track := &typesfakes.FakeLocalMediaTrack{}
		track.SignalCidReturns("cid")
		// directly add to publishedTracks without lock - for testing purpose only
		p.UpTrackManager.publishedTracks["cid"] = track

		p.AddTrack(&livekit.AddTrackRequest{
			Cid:  "cid",
			Name: "webcam",
			Type: livekit.TrackType_VIDEO,
		})
		require.Equal(t, 0, sink.WriteMessageCallCount())
	})

	t.Run("should not allow adding of duplicate tracks if already published by client id in sdp", func(t *testing.T) {
		p := newParticipantForTest("test")
		sink := p.params.Sink.(*routingfakes.FakeMessageSink)

		track := &typesfakes.FakeLocalMediaTrack{}
		track.SdpCidReturns("cid")
		// directly add to publishedTracks without lock - for testing purpose only
		p.UpTrackManager.publishedTracks["cid"] = track

		p.AddTrack(&livekit.AddTrackRequest{
			Cid:  "cid",
			Name: "webcam",
			Type: livekit.TrackType_VIDEO,
		})
		require.Equal(t, 0, sink.WriteMessageCallCount())
	})
}

func TestOutOfOrderUpdates(t *testing.T) {
	p := newParticipantForTest("test")
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
		msg := routing.NewMessageChannel()
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
		p.pendingTracks["cid"] = &pendingTrackInfo{TrackInfo: ti}

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

func TestConnectionQuality(t *testing.T) {

	// loss based score is currently a publisher method.
	videoScore := func(loss float32, numPublishing, numRegistered uint32) float32 {
		var reducedQuality bool
		if numRegistered > 0 && numPublishing != numRegistered {
			reducedQuality = true
		}
		return connectionquality.VideoConnectionScore(loss, reducedQuality)
	}

	testPublishedVideoTrack := func(loss float32, numPublishing, numRegistered uint32) *typesfakes.FakeLocalMediaTrack {
		tr := &typesfakes.FakeLocalMediaTrack{}
		score := videoScore(loss, numPublishing, numRegistered)
		t.Log("video score: ", score)
		tr.GetConnectionScoreReturns(score)
		return tr
	}

	testPublishedAudioTrack := func(totalPackets, packetsLost uint32) *typesfakes.FakeLocalMediaTrack {
		tr := &typesfakes.FakeLocalMediaTrack{}

		pctLoss := float32(0.0)
		if totalPackets > 0 {
			pctLoss = (float32(packetsLost) / float32(totalPackets)) * 100.0
		}
		score := connectionquality.AudioConnectionScore(pctLoss, 0, 0.0)
		t.Log("audio score: ", score)
		tr.GetConnectionScoreReturns(score)
		return tr
	}

	// TODO: this test is rather limited since we cannot mock DownTrack's Target & Max spatial layers
	// to improve this after split

	t.Run("smooth sailing", func(t *testing.T) {
		p := newParticipantForTest("test")
		p.UpTrackManager.publishedTracks["video"] = testPublishedVideoTrack(2, 3, 3)
		p.UpTrackManager.publishedTracks["audio"] = testPublishedAudioTrack(1000, 0)

		require.Equal(t, livekit.ConnectionQuality_EXCELLENT, p.GetConnectionQuality().GetQuality())
	})

	t.Run("reduced publishing", func(t *testing.T) {
		p := newParticipantForTest("test")
		p.UpTrackManager.publishedTracks["video"] = testPublishedVideoTrack(3, 2, 3)
		p.UpTrackManager.publishedTracks["audio"] = testPublishedAudioTrack(1000, 100)

		require.Equal(t, livekit.ConnectionQuality_GOOD, p.GetConnectionQuality().GetQuality())
	})

	t.Run("audio smooth publishing", func(t *testing.T) {
		p := newParticipantForTest("test")
		p.UpTrackManager.publishedTracks["audio"] = testPublishedAudioTrack(1000, 10)

		require.Equal(t, livekit.ConnectionQuality_EXCELLENT, p.GetConnectionQuality().GetQuality())
	})

	t.Run("audio reduced publishing", func(t *testing.T) {
		p := newParticipantForTest("test")
		p.UpTrackManager.publishedTracks["audio"] = testPublishedAudioTrack(1000, 100)

		require.Equal(t, livekit.ConnectionQuality_GOOD, p.GetConnectionQuality().GetQuality())
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

type participantOpts struct {
	permissions     *livekit.ParticipantPermission
	protocolVersion types.ProtocolVersion
}

func newParticipantForTestWithOpts(identity livekit.ParticipantIdentity, opts *participantOpts) *ParticipantImpl {
	if opts == nil {
		opts = &participantOpts{}
	}
	if opts.protocolVersion == 0 {
		opts.protocolVersion = 6
	}
	conf, _ := config.NewConfig("", nil)
	// disable mux, it doesn't play too well with unit test
	conf.RTC.UDPPort = 0
	conf.RTC.TCPPort = 0
	rtcConf, err := NewWebRTCConfig(conf, "")
	if err != nil {
		panic(err)
	}
	p, _ := NewParticipant(ParticipantParams{
		Identity:          identity,
		Config:            rtcConf,
		Sink:              &routingfakes.FakeMessageSink{},
		ProtocolVersion:   opts.protocolVersion,
		PLIThrottleConfig: conf.RTC.PLIThrottle,
		Grants: &auth.ClaimGrants{
			Video: &auth.VideoGrant{},
		},
	}, opts.permissions)
	return p
}

func newParticipantForTest(identity livekit.ParticipantIdentity) *ParticipantImpl {
	return newParticipantForTestWithOpts(identity, nil)
}

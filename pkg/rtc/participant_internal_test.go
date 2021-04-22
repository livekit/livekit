package rtc

import (
	"testing"
	"time"

	"github.com/pion/webrtc/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/routing/routingfakes"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/rtc/types/typesfakes"
	livekit "github.com/livekit/livekit-server/proto"
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
			assert.Equal(t, test.ready, p.IsReady())
		})
	}
}

func TestICEStateChange(t *testing.T) {
	t.Run("onClose gets called when ICE disconnected", func(t *testing.T) {
		p := newParticipantForTest("test")
		closeChan := make(chan bool, 1)
		p.onClose = func(participant types.Participant) {
			close(closeChan)
		}
		p.handlePublisherICEStateChange(webrtc.ICEConnectionStateDisconnected)

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
		track := &typesfakes.FakePublishedTrack{}
		track.IDReturns("id")
		published := false
		updated := false
		p.OnTrackUpdated(func(p types.Participant, track types.PublishedTrack) {
			updated = true
		})
		p.OnTrackPublished(func(p types.Participant, track types.PublishedTrack) {
			published = true
		})
		p.handleTrackPublished(track)

		assert.True(t, published)
		assert.False(t, updated)
		assert.Len(t, p.publishedTracks, 1)

		track.OnCloseArgsForCall(0)()
		assert.Len(t, p.publishedTracks, 0)
		assert.True(t, updated)
	})

	t.Run("sends back trackPublished event", func(t *testing.T) {
		p := newParticipantForTest("test")
		//track := &typesfakes.FakePublishedTrack{}
		//track.IDReturns("id")
		sink := p.responseSink.(*routingfakes.FakeMessageSink)
		p.AddTrack("cid", "webcam", livekit.TrackType_VIDEO)
		assert.Equal(t, 1, sink.WriteMessageCallCount())
		res := sink.WriteMessageArgsForCall(0).(*livekit.SignalResponse)
		assert.IsType(t, &livekit.SignalResponse_TrackPublished{}, res.Message)
		published := res.Message.(*livekit.SignalResponse_TrackPublished).TrackPublished
		assert.Equal(t, "cid", published.Cid)
		assert.Equal(t, "webcam", published.Track.Name)
		assert.Equal(t, livekit.TrackType_VIDEO, published.Track.Type)
	})

	t.Run("should not allow adding of duplicate tracks", func(t *testing.T) {
		p := newParticipantForTest("test")
		//track := &typesfakes.FakePublishedTrack{}
		//track.IDReturns("id")
		sink := p.responseSink.(*routingfakes.FakeMessageSink)
		p.AddTrack("cid", "webcam", livekit.TrackType_VIDEO)
		p.AddTrack("cid", "duplicate", livekit.TrackType_AUDIO)

		assert.Equal(t, 1, sink.WriteMessageCallCount())
	})
}

// after disconnection, things should continue to function and not panic
func TestDisconnectTiming(t *testing.T) {
	t.Run("Negotiate doesn't panic after channel closed", func(t *testing.T) {
		p := newParticipantForTest("test")
		msg := routing.NewMessageChannel()
		p.responseSink = msg
		go func() {
			for msg := range msg.ReadChan() {
				t.Log("received message from chan", msg)
			}
		}()
		track := &typesfakes.FakePublishedTrack{}
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

func newParticipantForTest(identity string) *ParticipantImpl {
	conf, _ := config.NewConfig("")
	rtcConf, _ := NewWebRTCConfig(&conf.RTC, "")
	p, _ := NewParticipant(
		identity,
		rtcConf,
		&routingfakes.FakeMessageSink{},
		config.AudioConfig{},
		0)
	return p
}

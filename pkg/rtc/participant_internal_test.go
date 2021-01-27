package rtc

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/routing/routingfakes"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/rtc/types/typesfakes"
	"github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/livekit-server/proto/livekit"
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
			p := &ParticipantImpl{
				state: test.state,
			}
			assert.Equal(t, test.ready, p.IsReady())
		})
	}
}

func TestTrackPublishEvents(t *testing.T) {
	p := newParticipantForTest("test")
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
}

// after disconnection, things should continue to function and not panic
func TestDisconnectTiming(t *testing.T) {
	t.Run("negotiate doesn't fail after channel closed", func(t *testing.T) {
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

		// close channel and then try to negotiate
		msg.Close()
		p.negotiate()
	})
}

func newParticipantForTest(name string) *ParticipantImpl {
	p, _ := NewParticipant(
		utils.NewGuid(utils.ParticipantPrefix),
		name,
		&typesfakes.FakePeerConnection{},
		&routingfakes.FakeMessageSink{},
		ReceiverConfig{})
	return p
}

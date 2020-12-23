package rtc_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/pkg/rtc/rtcfakes"
	"github.com/livekit/livekit-server/proto/livekit"
)

const (
	numParticipants = 3
)

func TestRoomJoin(t *testing.T) {
	t.Run("joining returns existing participant data", func(t *testing.T) {
		rm := newRoomWithParticipants(t, numParticipants)
		p := newMockParticipant("new")

		rm.Join(p)

		// expect new participant to get a JoinReply
		participants := p.SendJoinResponseArgsForCall(0)
		assert.Len(t, participants, numParticipants)
		assert.Len(t, rm.GetParticipants(), numParticipants+1)
	})

	t.Run("subscribe to existing channels upon join", func(t *testing.T) {
		numExisting := 3
		rm := newRoomWithParticipants(t, numExisting)
		p := newMockParticipant("new")

		err := rm.Join(p)
		assert.NoError(t, err)

		stateChangeCB := p.OnStateChangeArgsForCall(0)
		assert.NotNil(t, stateChangeCB)
		p.StateReturns(livekit.ParticipantInfo_JOINED)
		stateChangeCB(p, livekit.ParticipantInfo_JOINING)

		// it should become a subscriber when connectivity changes
		for _, op := range rm.GetParticipants() {
			if p == op {
				continue
			}
			mockP := op.(*rtcfakes.FakeParticipant)
			assert.NotZero(t, mockP.AddSubscriberCallCount())
			// last call should be to add the newest participant
			assert.Equal(t, p, mockP.AddSubscriberArgsForCall(mockP.AddSubscriberCallCount()-1))
		}
	})

	t.Run("participant removal is broadcasted to others", func(t *testing.T) {
		rm := newRoomWithParticipants(t, numParticipants)
		participants := rm.GetParticipants()
		p := participants[0].(*rtcfakes.FakeParticipant)

		rm.RemoveParticipant(p.ID())
		time.Sleep(10 * time.Millisecond)

		for _, op := range participants {
			if op == p {
				assert.Zero(t, p.SendParticipantUpdateCallCount())
				continue
			}
			fakeP := op.(*rtcfakes.FakeParticipant)
			assert.Equal(t, 1, fakeP.SendParticipantUpdateCallCount())
		}
	})
}

func TestNewTrack(t *testing.T) {
	t.Run("new track should be added to connected participants", func(t *testing.T) {
		rm := newRoomWithParticipants(t, 4)
		participants := rm.GetParticipants()
		p0 := participants[0].(*rtcfakes.FakeParticipant)
		p0.StateReturns(livekit.ParticipantInfo_JOINING)
		p1 := participants[1].(*rtcfakes.FakeParticipant)
		p1.StateReturns(livekit.ParticipantInfo_DISCONNECTED)
		p2 := participants[2].(*rtcfakes.FakeParticipant)
		p2.StateReturns(livekit.ParticipantInfo_JOINED)
		p3 := participants[3].(*rtcfakes.FakeParticipant)

		// p3 adds track
		track := newMockTrack(livekit.TrackInfo_VIDEO, "webcam")
		trackCB := p3.OnTrackPublishedArgsForCall(0)
		assert.NotNil(t, trackCB)
		trackCB(p3, track)
		// only p2 should've been called
		assert.Equal(t, 1, track.AddSubscriberCallCount())
		assert.Equal(t, p2, track.AddSubscriberArgsForCall(0))
	})
}

func newRoomWithParticipants(t *testing.T, num int) *rtc.Room {
	rm, err := rtc.NewRoomForRequest(&livekit.CreateRoomRequest{}, &rtc.WebRTCConfig{})
	if err != nil {
		panic("could not create a room")
	}
	for i := 0; i < num; i++ {
		participant := newMockParticipant("")
		err := rm.Join(participant)
		assert.NoError(t, err)
		//rm.participants[participant.ID()] = participant
	}
	return rm
}

package rtc_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/pkg/rtc/types/typesfakes"
	"github.com/livekit/livekit-server/proto/livekit"
)

const (
	numParticipants = 3
	defaultDelay    = 10 * time.Millisecond
)

func TestRoomJoin(t *testing.T) {
	t.Run("joining returns existing participant data", func(t *testing.T) {
		rm := newRoomWithParticipants(t, numParticipants)
		p := newMockParticipant("new")

		rm.Join(p)

		// expect new participant to get a JoinReply
		info, participants := p.SendJoinResponseArgsForCall(0)
		assert.Equal(t, info.Sid, rm.Sid)
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
			mockP := op.(*typesfakes.FakeParticipant)
			assert.NotZero(t, mockP.AddSubscriberCallCount())
			// last call should be to add the newest participant
			assert.Equal(t, p, mockP.AddSubscriberArgsForCall(mockP.AddSubscriberCallCount()-1))
		}
	})

	t.Run("participant state change is broadcasted to others", func(t *testing.T) {
		rm := newRoomWithParticipants(t, 1)
		participants := rm.GetParticipants()
		p := participants[0].(*typesfakes.FakeParticipant)

		rm.RemoveParticipant(p.ID())
		p.OnStateChangeArgsForCall(0)(p, livekit.ParticipantInfo_ACTIVE)
		time.Sleep(defaultDelay)

		for _, op := range participants {
			if op == p {
				assert.Zero(t, p.SendParticipantUpdateCallCount())
				continue
			}
			fakeP := op.(*typesfakes.FakeParticipant)
			assert.Equal(t, 1, fakeP.SendParticipantUpdateCallCount())
		}
	})

	t.Run("cannot exceed max participants", func(t *testing.T) {
		rm := newRoomWithParticipants(t, 1)
		rm.MaxParticipants = 1
		p := newMockParticipant("second")

		err := rm.Join(p)
		assert.Equal(t, rtc.ErrMaxParticipantsExceeded, err)
	})
}

func TestRoomClosure(t *testing.T) {
	t.Run("room closes after participant leaves", func(t *testing.T) {
		rm := newRoomWithParticipants(t, 1)
		isClosed := false
		rm.OnClose(func() {
			isClosed = true
		})
		p := rm.GetParticipants()[0]
		rm.RemoveParticipant(p.ID())

		time.Sleep(defaultDelay)

		assert.Len(t, rm.GetParticipants(), 0)
		assert.True(t, isClosed)

		assert.Equal(t, rtc.ErrRoomClosed, rm.Join(p))
	})

	t.Run("room does not close before empty timeout", func(t *testing.T) {
		rm := newRoomWithParticipants(t, 0)
		isClosed := false
		rm.OnClose(func() {
			isClosed = true
		})

		rm.CloseIfEmpty()
		assert.False(t, isClosed)
	})

	t.Run("room closes after empty timeout", func(t *testing.T) {
		rm := newRoomWithParticipants(t, 0)
		isClosed := false
		rm.OnClose(func() {
			isClosed = true
		})
		rm.EmptyTimeout = 1

		time.Sleep(1010 * time.Millisecond)
		rm.CloseIfEmpty()
		assert.True(t, isClosed)
	})
}

func TestNewTrack(t *testing.T) {
	t.Run("new track should be added to ready participants", func(t *testing.T) {
		rm := newRoomWithParticipants(t, 3)
		participants := rm.GetParticipants()
		p0 := participants[0].(*typesfakes.FakeParticipant)
		p0.IsReadyReturns(false)
		p1 := participants[1].(*typesfakes.FakeParticipant)
		p1.IsReadyReturns(true)

		pub := participants[2].(*typesfakes.FakeParticipant)

		// p3 adds track
		track := newMockTrack(livekit.TrackType_VIDEO, "webcam")
		trackCB := pub.OnTrackPublishedArgsForCall(0)
		assert.NotNil(t, trackCB)
		trackCB(pub, track)
		// only p2 should've been called
		assert.Equal(t, 1, track.AddSubscriberCallCount())
		assert.Equal(t, p1, track.AddSubscriberArgsForCall(0))
	})
}

func newRoomWithParticipants(t *testing.T, num int) *rtc.Room {
	rm := rtc.NewRoom(
		&livekit.Room{Name: "name"},
		rtc.WebRTCConfig{},
	)
	for i := 0; i < num; i++ {
		participant := newMockParticipant("")
		err := rm.Join(participant)
		assert.NoError(t, err)
		//rm.participants[participant.ID()] = participant
	}
	return rm
}

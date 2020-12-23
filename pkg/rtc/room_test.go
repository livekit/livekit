package rtc_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/pkg/rtc/rtcfakes"
	"github.com/livekit/livekit-server/proto/livekit"
)

func TestRoomJoin(t *testing.T) {
	t.Run("joining returns existing participant data", func(t *testing.T) {
		numExisting := 3
		rm := newRoomWithParticipants(t, numExisting)
		p := newMockParticipant("new")

		rm.Join(p)

		// expect new participant to get a JoinReply
		participants := p.SendJoinResponseArgsForCall(0)
		assert.Len(t, participants, numExisting)
		assert.Len(t, rm.GetParticipants(), numExisting+1)
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

package rtc

import (
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"

	"github.com/livekit/livekit-server/proto/livekit"
)

func TestRoomJoin(t *testing.T) {
	mockCtrl := gomock.NewController(t)
	defer mockCtrl.Finish()

	t.Run("joining returns existing participant data", func(t *testing.T) {
		numExisting := 3
		rm := newRoomWithParticipants(mockCtrl, numExisting)
		assert.NotNil(t, rm)
		p := newMockParticipant(mockCtrl)
		assert.NotNil(t, p)

		// expect new participant to get a JoinReply
		p.EXPECT().SendJoinResponse(gomock.Any()).Return(nil).Do(
			func(otherParticipants []Participant) {
				assert.Len(t, otherParticipants, numExisting)
			})

		rm.Join(p)

		assert.Len(t, rm.participants, numExisting+1)
	})
}

func newRoomWithParticipants(mockCtrl *gomock.Controller, num int) *Room {
	rm, err := NewRoomForRequest(&livekit.CreateRoomRequest{}, &WebRTCConfig{})
	if err != nil {
		panic("could not create a room")
	}
	for i := 0; i < num; i++ {
		participant := newMockParticipant(mockCtrl)
		rm.participants[participant.ID()] = participant
	}
	return rm
}

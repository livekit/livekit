package rtc

import (
	"fmt"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"

	"github.com/livekit/livekit-server/proto/livekit"
)

func TestRoomJoin(t *testing.T) {
	mockCtrl := gomock.NewController(t)
	defer mockCtrl.Finish()

	t.Run("joining returns existing participant data", func(t *testing.T) {
		rm := newRoomWithParticipants(t, mockCtrl, 3)
		assert.NotNil(t, rm)
		pc := newMockPeerConnection(mockCtrl)
		sc := NewMockSignalConnection(mockCtrl)
		p, err := NewParticipant(pc, sc, "newparticipant")
		assert.NoError(t, err)
		assert.NotNil(t, p)

		// expect new participant to get a JoinReply
	})
}

func newRoomWithParticipants(t *testing.T, mockCtrl *gomock.Controller, num int) *Room {
	rm, err := NewRoomForRequest(&livekit.CreateRoomRequest{}, &WebRTCConfig{})
	if err != nil {
		panic("could not create a room")
	}
	for i := 0; i < num; i++ {
		pc := newMockPeerConnection(mockCtrl)
		participant, err := NewParticipant(pc,
			NewMockSignalConnection(mockCtrl),
			fmt.Sprintf("p%d", i))
		assert.NoError(t, err, "could not create participant for room")
		rm.participants[participant.ID()] = participant
	}
	return rm
}

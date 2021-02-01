package service_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/livekit/livekit-server/pkg/service"
	"github.com/livekit/livekit-server/proto/livekit"
)

func TestParticipantPersisence(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
		return
	}
	rs := service.NewRedisRoomStore(redisClient())

	roomName := "room1"
	rs.DeleteRoom(roomName)

	p := &livekit.ParticipantInfo{
		Sid:      "PA_test",
		Identity: "test",
		State:    livekit.ParticipantInfo_ACTIVE,
		Tracks: []*livekit.TrackInfo{
			{
				Sid:  "track1",
				Type: livekit.TrackType_AUDIO,
				Name: "audio",
			},
		},
	}

	// create the participant
	assert.NoError(t, rs.PersistParticipant(roomName, p))

	// result should match
	pGet, err := rs.GetParticipant(roomName, p.Identity)
	assert.NoError(t, err)
	assert.Equal(t, p.Identity, pGet.Identity)
	assert.Equal(t, len(p.Tracks), len(pGet.Tracks))
	assert.Equal(t, p.Tracks[0].Sid, pGet.Tracks[0].Sid)

	// list should return one participant
	participants, err := rs.ListParticipants(roomName)
	assert.NoError(t, err)
	assert.Len(t, participants, 1)

	// deleting participant should return back to normal
	assert.NoError(t, rs.DeleteParticipant(roomName, p.Identity))

	participants, err = rs.ListParticipants(roomName)
	assert.NoError(t, err)
	assert.Len(t, participants, 0)

	// shouldn't be able to get it
	_, err = rs.GetParticipant(roomName, p.Identity)
	assert.Equal(t, err, service.ErrParticipantNotFound)
}

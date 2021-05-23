package service_test

import (
	"testing"

	"github.com/livekit/livekit-server/pkg/service"
	livekit "github.com/livekit/livekit-server/proto"
	"github.com/stretchr/testify/require"
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
	require.NoError(t, rs.PersistParticipant(roomName, p))

	// result should match
	pGet, err := rs.GetParticipant(roomName, p.Identity)
	require.NoError(t, err)
	require.Equal(t, p.Identity, pGet.Identity)
	require.Equal(t, len(p.Tracks), len(pGet.Tracks))
	require.Equal(t, p.Tracks[0].Sid, pGet.Tracks[0].Sid)

	// list should return one participant
	participants, err := rs.ListParticipants(roomName)
	require.NoError(t, err)
	require.Len(t, participants, 1)

	// deleting participant should return back to normal
	require.NoError(t, rs.DeleteParticipant(roomName, p.Identity))

	participants, err = rs.ListParticipants(roomName)
	require.NoError(t, err)
	require.Len(t, participants, 0)

	// shouldn't be able to get it
	_, err = rs.GetParticipant(roomName, p.Identity)
	require.Equal(t, err, service.ErrParticipantNotFound)
}

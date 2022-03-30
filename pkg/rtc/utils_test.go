package rtc

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
)

func TestPackStreamId(t *testing.T) {
	packed := "PA_123abc|uuid-id"
	pID, trackID := UnpackStreamID(packed)
	require.Equal(t, livekit.ParticipantID("PA_123abc"), pID)
	require.Equal(t, livekit.TrackID("uuid-id"), trackID)

	require.Equal(t, packed, PackStreamID(pID, trackID))
}

func TestPackDataTrackLabel(t *testing.T) {
	pID := livekit.ParticipantID("PA_123abc")
	trackID := livekit.TrackID("TR_b3da25")
	label := "trackLabel"
	packed := "PA_123abc|TR_b3da25|trackLabel"
	require.Equal(t, packed, PackDataTrackLabel(pID, trackID, label))

	p, tr, l := UnpackDataTrackLabel(packed)
	require.Equal(t, pID, p)
	require.Equal(t, trackID, tr)
	require.Equal(t, label, l)
}

package rtc

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPackStreamId(t *testing.T) {
	packed := "PA_123abc|uuid-id"
	pId, trackId := UnpackStreamID(packed)
	require.Equal(t, "PA_123abc", pId)
	require.Equal(t, "uuid-id", trackId)

	require.Equal(t, packed, PackStreamID(pId, trackId))
}

func TestPackDataTrackLabel(t *testing.T) {
	pId := "PA_123abc"
	trackId := "TR_b3da25"
	label := "trackLabel"
	packed := "PA_123abc|TR_b3da25|trackLabel"
	require.Equal(t, packed, PackDataTrackLabel(pId, trackId, label))

	p, tr, l := UnpackDataTrackLabel(packed)
	require.Equal(t, pId, p)
	require.Equal(t, trackId, tr)
	require.Equal(t, label, l)
}

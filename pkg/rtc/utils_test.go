package rtc

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPackTrackId(t *testing.T) {
	packed := "PA_123abc|uuid-id"
	pId, trackId := UnpackTrackId(packed)
	assert.Equal(t, "PA_123abc", pId)
	assert.Equal(t, "uuid-id", trackId)

	assert.Equal(t, packed, PackTrackId(pId, trackId))
}

func TestPackDataTrackLabel(t *testing.T) {
	pId := "PA_123abc"
	trackId := "TR_b3da25"
	label := "trackLabel"
	packed := "PA_123abc|TR_b3da25|trackLabel"
	assert.Equal(t, packed, PackDataTrackLabel(pId, trackId, label))

	p, tr, l := UnpackDataTrackLabel(packed)
	assert.Equal(t, pId, p)
	assert.Equal(t, trackId, tr)
	assert.Equal(t, label, l)
}

package codecmunger

import (
	"errors"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
)

var (
	ErrNotVP8                          = errors.New("not VP8")
	ErrOutOfOrderVP8PictureIdCacheMiss = errors.New("out-of-order VP8 picture id not found in cache")
	ErrFilteredVP8TemporalLayer        = errors.New("filtered VP8 temporal layer")
)

/* RAJA-REMOVE
type VideoLayerSelectorResult struct {
	IsSelected              bool
	IsRelevant              bool
	IsSwitchingLayer        bool
	IsSwitchingToMaxSpatial bool
	RTPMarker               bool
}
*/

type CodecMunger interface {
	GetState() interface{}
	SeedState(state interface{})

	SetLast(extPkt *buffer.ExtPacket)
	UpdateOffsets(extPkt *buffer.ExtPacket)

	UpdateAndGet(extPkt *buffer.ExtPacket, snOutOfOrder bool, snHasGap bool, maxTemporal int32) (interface{}, error)

	UpdateAndGetPadding(newPicture bool) interface{}
}

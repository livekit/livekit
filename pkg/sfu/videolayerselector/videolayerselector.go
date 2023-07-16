package videolayerselector

import (
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/videolayerselector/temporallayerselector"
)

type VideoLayerSelectorResult struct {
	IsSelected                    bool
	IsRelevant                    bool
	IsSwitching                   bool
	IsResuming                    bool
	RTPMarker                     bool
	DependencyDescriptorExtension []byte
}

type VideoLayerSelector interface {
	IsOvershootOkay() bool

	SetTemporalLayerSelector(tls temporallayerselector.TemporalLayerSelector)

	SetMax(maxLayer buffer.VideoLayer)
	SetMaxSpatial(layer int32)
	SetMaxTemporal(layer int32)
	GetMax() buffer.VideoLayer

	SetTarget(targetLayer buffer.VideoLayer)
	GetTarget() buffer.VideoLayer

	SetRequestSpatial(layer int32)
	GetRequestSpatial() int32

	CheckSync() (locked bool, layer int32)

	SetMaxSeen(maxSeenLayer buffer.VideoLayer)
	SetMaxSeenSpatial(layer int32)
	SetMaxSeenTemporal(layer int32)
	GetMaxSeen() buffer.VideoLayer

	SetParked(parkedLayer buffer.VideoLayer)
	GetParked() buffer.VideoLayer

	SetCurrent(currentLayer buffer.VideoLayer)
	GetCurrent() buffer.VideoLayer

	Select(extPkt *buffer.ExtPacket, layer int32) VideoLayerSelectorResult
	SelectTemporal(extPkt *buffer.ExtPacket) (int32, bool)
	Rollback()
}

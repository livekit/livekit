package videolayerselector

import "github.com/livekit/livekit-server/pkg/sfu/buffer"

type VideoLayerSelectorResult struct {
	IsSelected              bool
	IsRelevant              bool
	IsSwitchingLayer        bool
	IsSwitchingToMaxSpatial bool
	RTPMarker               bool
}

type VideoLayerSelector interface {
	SetMax(maxLayer buffer.VideoLayer)
	SetMaxSpatial(layer int32)
	SetMaxTemporal(layer int32)
	GetMax() buffer.VideoLayer

	SetTarget(targetLayer buffer.VideoLayer)
	GetTarget() buffer.VideoLayer

	SetRequestSpatial(layer int32)
	GetRequestSpatial() int32

	SetMaxSeen(maxSeenLayer buffer.VideoLayer)
	SetMaxSeenSpatial(layer int32)
	SetMaxSeenTemporal(layer int32)
	GetMaxSeen() buffer.VideoLayer

	SetParked(parkedLayer buffer.VideoLayer)
	GetParked() buffer.VideoLayer

	SetCurrent(currentLayer buffer.VideoLayer)
	GetCurrent() buffer.VideoLayer

	Select(extPkt *buffer.ExtPacket, layer int32) VideoLayerSelectorResult
}

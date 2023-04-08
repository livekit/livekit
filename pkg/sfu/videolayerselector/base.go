package videolayerselector

import (
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/videolayerselector/temporallayerselector"
	"github.com/livekit/protocol/logger"
)

type Base struct {
	logger logger.Logger

	tls temporallayerselector.TemporalLayerSelector

	maxLayer       buffer.VideoLayer
	targetLayer    buffer.VideoLayer
	requestSpatial int32
	maxSeenLayer   buffer.VideoLayer

	parkedLayer buffer.VideoLayer

	currentLayer buffer.VideoLayer
}

func NewBase(logger logger.Logger) *Base {
	return &Base{
		logger:         logger,
		maxLayer:       buffer.InvalidLayer,
		targetLayer:    buffer.InvalidLayer, // start off with nothing, let streamallocator/opportunistic forwarder set the target
		requestSpatial: buffer.InvalidLayerSpatial,
		maxSeenLayer:   buffer.InvalidLayer,
		parkedLayer:    buffer.InvalidLayer,
		currentLayer:   buffer.InvalidLayer,
	}
}

func (b *Base) IsOvershootOkay() bool {
	return false
}

func (b *Base) SetTemporalLayerSelector(tls temporallayerselector.TemporalLayerSelector) {
	b.tls = tls
}

func (b *Base) SetMax(maxLayer buffer.VideoLayer) {
	b.maxLayer = maxLayer
}

func (b *Base) SetMaxSpatial(layer int32) {
	b.maxLayer.Spatial = layer
}

func (b *Base) SetMaxTemporal(layer int32) {
	b.maxLayer.Temporal = layer
}

func (b *Base) GetMax() buffer.VideoLayer {
	return b.maxLayer
}

func (b *Base) SetTarget(targetLayer buffer.VideoLayer) {
	b.targetLayer = targetLayer
}

func (b *Base) GetTarget() buffer.VideoLayer {
	return b.targetLayer
}

func (b *Base) SetRequestSpatial(layer int32) {
	b.requestSpatial = layer
}

func (b *Base) GetRequestSpatial() int32 {
	return b.requestSpatial
}

func (b *Base) SetMaxSeen(maxSeenLayer buffer.VideoLayer) {
	b.maxSeenLayer = maxSeenLayer
}

func (b *Base) SetMaxSeenSpatial(layer int32) {
	b.maxSeenLayer.Spatial = layer
}

func (b *Base) SetMaxSeenTemporal(layer int32) {
	b.maxSeenLayer.Temporal = layer
}

func (b *Base) GetMaxSeen() buffer.VideoLayer {
	return b.maxSeenLayer
}

func (b *Base) SetParked(parkedLayer buffer.VideoLayer) {
	b.parkedLayer = parkedLayer
}

func (b *Base) GetParked() buffer.VideoLayer {
	return b.parkedLayer
}

func (b *Base) SetCurrent(currentLayer buffer.VideoLayer) {
	b.currentLayer = currentLayer
}

func (b *Base) GetCurrent() buffer.VideoLayer {
	return b.currentLayer
}

func (b *Base) Select(_extPkt *buffer.ExtPacket, _layer int32) (result VideoLayerSelectorResult) {
	return
}

func (b *Base) SelectTemporal(extPkt *buffer.ExtPacket) int32 {
	if b.tls != nil {
		this, next := b.tls.Select(extPkt, b.currentLayer.Temporal, b.targetLayer.Temporal)
		b.currentLayer.Temporal = next
		return this
	}

	return b.currentLayer.Temporal
}

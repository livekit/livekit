package videolayerselector

import (
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/sfu/videolayerselector/temporallayerselector"
	"github.com/livekit/protocol/logger"
)

type Base struct {
	logger logger.Logger

	tls temporallayerselector.TemporalLayerSelector

	maxLayer     buffer.VideoLayer
	maxSeenLayer buffer.VideoLayer

	targetLayer         buffer.VideoLayer
	previousTargetLayer buffer.VideoLayer

	requestSpatial int32

	parkedLayer         buffer.VideoLayer
	previousParkedLayer buffer.VideoLayer

	currentLayer  buffer.VideoLayer
	previousLayer buffer.VideoLayer
}

func NewBase(logger logger.Logger) *Base {
	return &Base{
		logger:              logger,
		maxLayer:            buffer.InvalidLayer,
		maxSeenLayer:        buffer.InvalidLayer,
		targetLayer:         buffer.InvalidLayer, // start off with nothing, let streamallocator/opportunistic forwarder set the target
		previousTargetLayer: buffer.InvalidLayer,
		requestSpatial:      buffer.InvalidLayerSpatial,
		parkedLayer:         buffer.InvalidLayer,
		previousParkedLayer: buffer.InvalidLayer,
		currentLayer:        buffer.InvalidLayer,
		previousLayer:       buffer.InvalidLayer,
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
	b.previousTargetLayer = targetLayer
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

func (b *Base) CheckSync() (locked bool, layer int32) {
	layer = b.GetRequestSpatial()
	locked = layer == b.GetCurrent().Spatial || b.GetParked().IsValid()
	return
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

func (b *Base) Rollback() {
	b.logger.Infow(
		"rolling back",
		"previous", b.previousLayer,
		"current", b.currentLayer,
		"previousParked", b.previousParkedLayer,
		"parked", b.parkedLayer,
		"previousTarget", b.previousTargetLayer,
		"target", b.targetLayer,
		"max", b.maxLayer,
		"req", b.requestSpatial,
		"maxSeen", b.maxSeenLayer,
	)
	b.parkedLayer = b.previousParkedLayer
	b.currentLayer = b.previousLayer
	b.targetLayer = b.previousTargetLayer
}

func (b *Base) SelectTemporal(extPkt *buffer.ExtPacket) (int32, bool) {
	if b.tls != nil {
		isSwitching := false
		this, next := b.tls.Select(extPkt, b.currentLayer.Temporal, b.targetLayer.Temporal)
		if next != b.currentLayer.Temporal {
			isSwitching = true

			b.previousLayer = b.currentLayer
			b.currentLayer.Temporal = next

			b.logger.Infow(
				"updating temporal layer",
				"previous", b.previousLayer,
				"current", b.currentLayer,
				"previousParked", b.previousParkedLayer,
				"parked", b.parkedLayer,
				"previousTarget", b.previousTargetLayer,
				"target", b.targetLayer,
				"max", b.maxLayer,
				"req", b.requestSpatial,
				"maxSeen", b.maxSeenLayer,
			)
		}
		return this, isSwitching
	}

	return b.currentLayer.Temporal, false
}

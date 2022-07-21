package buffer

import (
	"fmt"

	"github.com/pion/rtp"

	dd "github.com/livekit/livekit-server/pkg/sfu/dependencydescriptor"

	"github.com/livekit/protocol/logger"
)

type DependencyDescriptorParser struct {
	structure         *dd.FrameDependencyStructure
	ddExt             uint8
	logger            logger.Logger
	onMaxLayerChanged func(int32, int32)
	decodeTargetLayer []VideoLayer
}

func NewDependencyDescriptorParser(ddExt uint8, logger logger.Logger, onMaxLayerChanged func(int32, int32)) *DependencyDescriptorParser {
	logger.Infow("creating dependency descriptor parser", "ddExt", ddExt)
	return &DependencyDescriptorParser{
		ddExt:             ddExt,
		logger:            logger,
		onMaxLayerChanged: onMaxLayerChanged,
	}
}

func (r *DependencyDescriptorParser) Parse(pkt *rtp.Packet) (*dd.DependencyDescriptor, VideoLayer, error) {
	var videoLayer VideoLayer
	if ddBuf := pkt.GetExtension(r.ddExt); ddBuf != nil {
		var ddVal dd.DependencyDescriptor
		ext := &dd.DependencyDescriptorExtension{
			Descriptor: &ddVal,
			Structure:  r.structure,
		}
		_, err := ext.Unmarshal(ddBuf)
		if err != nil {
			// r.logger.Debugw("failed to parse generic dependency descriptor", "err", err, "payload", pkt.PayloadType, "ddbufLen", len(ddBuf))
			return nil, videoLayer, err
		}

		if ddVal.FrameDependencies != nil {
			videoLayer.Spatial, videoLayer.Temporal = int32(ddVal.FrameDependencies.SpatialId), int32(ddVal.FrameDependencies.TemporalId)
		}
		if ddVal.AttachedStructure != nil && !ddVal.FirstPacketInFrame {
			// r.logger.Debugw("ignoring non-first packet in frame with attached structure")
			return nil, videoLayer, nil
		}

		if ddVal.AttachedStructure != nil {
			var maxSpatial, maxTemporal int32
			r.structure = ddVal.AttachedStructure
			r.decodeTargetLayer = r.decodeTargetLayer[:0]
			for target := 0; target < r.structure.NumDecodeTargets; target++ {
				layer := VideoLayer{0, 0}
				for _, t := range r.structure.Templates {
					if t.DecodeTargetIndications[target] != dd.DecodeTargetNotPresent {
						if layer.Spatial < int32(t.SpatialId) {
							layer.Spatial = int32(t.SpatialId)
						}
						if layer.Temporal < int32(t.TemporalId) {
							layer.Temporal = int32(t.TemporalId)
						}
					}
				}
				if layer.Spatial > maxSpatial {
					maxSpatial = layer.Spatial
				}
				if layer.Temporal > maxTemporal {
					maxTemporal = layer.Temporal
				}
				r.decodeTargetLayer = append(r.decodeTargetLayer, layer)
			}
			r.logger.Debugw("max layer changed", "maxSpatial", maxSpatial, "maxTemporal", maxTemporal)
			go r.onMaxLayerChanged(maxSpatial, maxTemporal)
		}

		if ddVal.AttachedStructure != nil && ddVal.FirstPacketInFrame {
			r.logger.Debugw(fmt.Sprintf("parsed dependency descriptor\n%s", ddVal.String()))
		}

		if mask := ddVal.ActiveDecodeTargetsBitmask; mask != nil {
			var maxSpatial, maxTemporal int32
			for dt, layer := range r.decodeTargetLayer {
				if *mask&(1<<dt) != uint32(dd.DecodeTargetNotPresent) {
					if maxSpatial < layer.Spatial {
						maxSpatial = layer.Spatial
					}
					if maxTemporal < layer.Temporal {
						maxTemporal = layer.Temporal
					}
				}
			}
			r.logger.Debugw("max layer changed", "maxSpatial", maxSpatial, "maxTemporal", maxTemporal)
			r.onMaxLayerChanged(maxSpatial, maxTemporal)
		}
		return &ddVal, videoLayer, nil
	}
	return nil, videoLayer, nil
}

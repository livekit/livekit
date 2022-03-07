package buffer

const (
	InvalidLayerSpatial  = int32(-1)
	InvalidLayerTemporal = int32(-1)

	DefaultMaxLayerSpatial  = int32(2)
	DefaultMaxLayerTemporal = int32(3)
)

type Bitrates [DefaultMaxLayerSpatial + 1][DefaultMaxLayerTemporal + 1]int64

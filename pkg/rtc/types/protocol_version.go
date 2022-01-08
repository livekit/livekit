package types

type ProtocolVersion int

const DefaultProtocol = 2

func (v ProtocolVersion) SupportsPackedStreamId() bool {
	return v > 0
}

func (v ProtocolVersion) SupportsProtobuf() bool {
	return v > 0
}

func (v ProtocolVersion) HandlesDataPackets() bool {
	return v > 1
}

// SubscriberAsPrimary indicates clients initiate subscriber connection as primary
func (v ProtocolVersion) SubscriberAsPrimary() bool {
	return v > 2
}

// SupportsSpeakerChanged - if client handles speaker info deltas, instead of a comprehensive list
func (v ProtocolVersion) SupportsSpeakerChanged() bool {
	return v > 2
}

// SupportsTransceiverReuse - if transceiver reuse is supported, optimizes SDP size
func (v ProtocolVersion) SupportsTransceiverReuse() bool {
	return v > 3
}

// SupportsConnectionQuality - avoid sending frequent ConnectionQuality updates for lower protocol versions
func (v ProtocolVersion) SupportsConnectionQuality() bool {
	return v > 4
}

func (v ProtocolVersion) SupportsSessionMigrate() bool {
	return v > 5
}

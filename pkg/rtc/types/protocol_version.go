package types

type ProtocolVersion int

const CurrentProtocol = 8

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

func (v ProtocolVersion) SupportsICELite() bool {
	return v > 5
}

func (v ProtocolVersion) SupportsUnpublish() bool {
	return v > 6
}

// SupportFastStart - if client supports fast start, server side will send media streams
// in the first offer
func (v ProtocolVersion) SupportFastStart() bool {
	return v > 7
}

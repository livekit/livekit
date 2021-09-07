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

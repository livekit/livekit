package types

type ProtocolVersion int

func (v ProtocolVersion) SupportsPackedStreamId() bool {
	return v > 0
}

func (v ProtocolVersion) HandlesDataPackets() bool {
	return v > 1
}

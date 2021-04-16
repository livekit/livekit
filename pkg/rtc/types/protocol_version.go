package types

type ProtocolVersion int

func (v ProtocolVersion) SupportsPackedStreamId() bool {
	return v > 0
}

package sendsidebwe

import "github.com/livekit/protocol/logger"

type SendSideBWE struct {
	*TransportWideSequenceNumber
	*PacketTracker
}

func NewSendSideBWE(logger logger.Logger) *SendSideBWE {
	return &SendSideBWE{
		TransportWideSequenceNumber: NewTransportWideSequenceNumber(),
		PacketTracker:               NewPacketTracker(),
	}
}

// ------------------------------------------------

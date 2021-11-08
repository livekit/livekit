package buffer

import "errors"

var (
	errPacketNotFound = errors.New("packet not found in cache")
	errBufferTooSmall = errors.New("buffer too small")
	errPacketTooOld   = errors.New("received packet too old")
	errRTXPacket      = errors.New("packet already received")
)

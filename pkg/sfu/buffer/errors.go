package buffer

import "errors"

var (
	ErrPacketNotFound = errors.New("packet not found in cache")
	ErrBufferTooSmall = errors.New("buffer too small")
	ErrPacketTooOld   = errors.New("received packet too old")
	ErrRTXPacket      = errors.New("packet already received")
)

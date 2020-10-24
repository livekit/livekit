package sfu

import "errors"

var (
	errPeerConnectionInitFailed = errors.New("pc init failed")
	errPtNotSupported           = errors.New("payload type not supported")
	// router errors
	errNoReceiverFound = errors.New("no receiver found")
	// Helpers errors
	errShortPacket = errors.New("packet is not large enough")
	errNilPacket   = errors.New("invalid nil packet")
	// buffer errors
	errPacketNotFound = errors.New("packet not found in cache")
	errPacketTooOld   = errors.New("packet not found in cache, too old")
)

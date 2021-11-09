package sfu

import "errors"

var (
	// PeerLocal erors
	errPeerConnectionInitFailed = errors.New("pc init failed")
	errCreatingDataChannel      = errors.New("failed to create data channel")
	// router errors
	errNoReceiverFound = errors.New("no receiver found")
	// Helpers errors
	errShortPacket = errors.New("packet is not large enough")
	errNilPacket   = errors.New("invalid nil packet")

	ErrSpatialNotSupported  = errors.New("current track does not support simulcast/SVC")
	ErrSpatialLayerNotFound = errors.New("the requested layer does not exist")
	ErrSpatialLayerBusy     = errors.New("a spatial layer change is in progress, try later")
)

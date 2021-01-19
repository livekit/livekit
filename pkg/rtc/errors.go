package rtc

import "errors"

var (
	ErrInvalidRoomName         = errors.New("room must have a unique name")
	ErrRoomNotFound            = errors.New("requested room does not exist")
	ErrRoomClosed              = errors.New("room has already closed")
	ErrPermissionDenied        = errors.New("no permissions to access the room")
	ErrMaxParticipantsExceeded = errors.New("room has exceeded its max participants")
	ErrUnexpectedOffer         = errors.New("expected answer SDP, received offer")
	ErrUnexpectedNegotiation   = errors.New("client negotiation has not been granted")
)

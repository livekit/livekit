package rtc

import "errors"

var (
	ErrRoomClosed              = errors.New("room has already closed")
	ErrPermissionDenied        = errors.New("no permissions to access the room")
	ErrMaxParticipantsExceeded = errors.New("room has exceeded its max participants")
	ErrAlreadyJoined           = errors.New("a participant with the same identity is already in the room")
	ErrUnexpectedOffer         = errors.New("expected answer SDP, received offer")
	ErrDataChannelUnavailable  = errors.New("data channel is not available")
	ErrCannotSubscribe         = errors.New("participant does not have permission to subscribe")
)

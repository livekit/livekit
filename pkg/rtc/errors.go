package rtc

import "errors"

var (
	ErrRoomIdMissing    = errors.New("room is not passed in")
	ErrInvalidRoomName  = errors.New("room must have a unique name")
	ErrRoomNotFound     = errors.New("requested room does not exist")
	ErrPermissionDenied = errors.New("no permissions to access the room")
	ErrUnexpectedOffer  = errors.New("expected answer SDP, received offer")
)

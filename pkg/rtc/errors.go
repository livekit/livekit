package rtc

import "errors"

var (
	ErrRoomIdMissing    = errors.New("room is not passed in")
	ErrRoomNotFound     = errors.New("requested room does not exist")
	ErrPermissionDenied = errors.New("no permissions to access the room")
)

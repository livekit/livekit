package service

import "errors"

var (
	ErrRoomNotFound        = errors.New("requested room does not exist")
	ErrParticipantNotFound = errors.New("participant does not exist")
	ErrTrackNotFound       = errors.New("track is not found")
)

package service

import "errors"

var (
	ErrRoomNotFound         = errors.New("requested room does not exist")
	ErrRoomLockFailed       = errors.New("could not lock room")
	ErrRoomUnlockFailed     = errors.New("could not unlock room, lock token does not match")
	ErrParticipantNotFound  = errors.New("participant does not exist")
	ErrTrackNotFound        = errors.New("track is not found")
	ErrWebHookMissingAPIKey = errors.New("api_key is required to use webhooks")
	ErrUnsupportedSelector  = errors.New("unsupported node selector")
)

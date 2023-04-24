package rtc

import "errors"

var (
	ErrRoomClosed              = errors.New("room has already closed")
	ErrPermissionDenied        = errors.New("no permissions to access the room")
	ErrMaxParticipantsExceeded = errors.New("room has exceeded its max participants")
	ErrLimitExceeded           = errors.New("node has exceeded its configured limit")
	ErrAlreadyJoined           = errors.New("a participant with the same identity is already in the room")
	ErrDataChannelUnavailable  = errors.New("data channel is not available")
	ErrEmptyIdentity           = errors.New("participant identity cannot be empty")
	ErrEmptyParticipantID      = errors.New("participant ID cannot be empty")
	ErrMissingGrants           = errors.New("VideoGrant is missing")

	// Track subscription related
	ErrNoTrackPermission         = errors.New("participant is not allowed to subscribe to this track")
	ErrNoSubscribePermission     = errors.New("participant is not given permission to subscribe to tracks")
	ErrTrackNotFound             = errors.New("track cannot be found")
	ErrTrackNotAttached          = errors.New("track is not yet attached")
	ErrTrackNotBound             = errors.New("track not bound")
	ErrSubscriptionLimitExceeded = errors.New("participant has exceeded its subscription limit")
)

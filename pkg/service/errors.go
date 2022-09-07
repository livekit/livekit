package service

import "errors"

var (
	ErrEgressNotFound        = errors.New("egress does not exist")
	ErrEgressNotConnected    = errors.New("egress not connected (redis required)")
	ErrIdentityEmpty         = errors.New("identity cannot be empty")
	ErrIngressNotConnected   = errors.New("ingress not connected (redis required)")
	ErrIngressNotFound       = errors.New("ingress does not exist")
	ErrMetadataExceedsLimits = errors.New("metadata size exceeds limits")
	ErrOperationFailed       = errors.New("operation cannot be completed")
	ErrParticipantNotFound   = errors.New("participant does not exist")
	ErrRoomNotFound          = errors.New("requested room does not exist")
	ErrRoomLockFailed        = errors.New("could not lock room")
	ErrRoomUnlockFailed      = errors.New("could not unlock room, lock token does not match")
	ErrTrackNotFound         = errors.New("track is not found")
	ErrWebHookMissingAPIKey  = errors.New("api_key is required to use webhooks")
)

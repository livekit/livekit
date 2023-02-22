package service

import (
	"github.com/livekit/psrpc"
)

var (
	ErrEgressNotFound        = psrpc.NewErrorf(psrpc.NotFound, "egress does not exist")
	ErrEgressNotConnected    = psrpc.NewErrorf(psrpc.Internal, "egress not connected (redis required)")
	ErrIdentityEmpty         = psrpc.NewErrorf(psrpc.InvalidArgument, "identity cannot be empty")
	ErrIngressNotConnected   = psrpc.NewErrorf(psrpc.Internal, "ingress not connected (redis required)")
	ErrIngressNotFound       = psrpc.NewErrorf(psrpc.NotFound, "ingress does not exist")
	ErrMetadataExceedsLimits = psrpc.NewErrorf(psrpc.InvalidArgument, "metadata size exceeds limits")
	ErrOperationFailed       = psrpc.NewErrorf(psrpc.Internal, "operation cannot be completed")
	ErrParticipantNotFound   = psrpc.NewErrorf(psrpc.NotFound, "participant does not exist")
	ErrRoomNotFound          = psrpc.NewErrorf(psrpc.NotFound, "requested room does not exist")
	ErrRoomLockFailed        = psrpc.NewErrorf(psrpc.Internal, "could not lock room")
	ErrRoomUnlockFailed      = psrpc.NewErrorf(psrpc.Internal, "could not unlock room, lock token does not match")
	ErrTrackNotFound         = psrpc.NewErrorf(psrpc.NotFound, "track is not found")
	ErrWebHookMissingAPIKey  = psrpc.NewErrorf(psrpc.InvalidArgument, "api_key is required to use webhooks")
)

package routing

import "errors"

var (
	ErrNotFound             = errors.New("could not find object")
	ErrIPNotSet             = errors.New("ip address is required and not set")
	ErrHandlerNotDefined    = errors.New("handler not defined")
	ErrIncorrectRTCNode     = errors.New("current node isn't the RTC node for the room")
	ErrNodeNotFound         = errors.New("could not locate the node")
	ErrNodeLimitReached     = errors.New("reached configured limit for node")
	ErrInvalidRouterMessage = errors.New("invalid router message")
	ErrChannelClosed        = errors.New("channel closed")
	ErrChannelFull          = errors.New("channel is full")
)

package routing

import "errors"

var (
	ErrNotFound             = errors.New("could not find object")
	ErrHandlerNotDefined    = errors.New("handler not defined")
	ErrNoAvailableNodes     = errors.New("could not find any available nodes")
	ErrIncorrectRTCNode     = errors.New("current node isn't the RTC node for the room")
	errInvalidRouterMessage = errors.New("invalid router message")
	ErrChannelClosed        = errors.New("channel closed")
	ErrChannelFull          = errors.New("channel is full")
)

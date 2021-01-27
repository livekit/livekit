package routing

import "errors"

var (
	ErrNotFound             = errors.New("could not find object")
	ErrHandlerNotDefined    = errors.New("handler not defined")
	ErrNoAvailableNodes     = errors.New("could not find any available nodes")
	errInvalidRouterMessage = errors.New("invalid router message")
	ErrChannelClosed        = errors.New("channel closed")
)

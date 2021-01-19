package routing

import "errors"

var (
	ErrNotFound             = errors.New("could not find object")
	ErrHandlerNotDefined    = errors.New("handler not defined")
	ErrNoAvailableNodes     = errors.New("could not find any available nodes")
	ErrIncorrectNodeForRoom = errors.New("incorrect node for the current room")
	errInvalidRouterMessage = errors.New("invalid router message")
)

package routing

import "errors"

var (
	ErrNodeNotFound      = errors.New("could not find node")
	ErrHandlerNotDefined = errors.New("handler not defined")
)

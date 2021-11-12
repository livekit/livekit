package sfu

import "errors"

var (
	ErrSpatialNotSupported  = errors.New("current track does not support simulcast/SVC")
	ErrSpatialLayerNotFound = errors.New("the requested layer does not exist")
)

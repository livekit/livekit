package service

import "errors"

var (
	ErrRoomNotFound      = errors.New("requested room does not exist")
	ErrNoRegisteredNodes = errors.New("there are no registered nodes")
)

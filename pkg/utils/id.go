package utils

import (
	"github.com/lithammer/shortuuid/v3"
)

const (
	RoomPrefix        = "R-"
	NodePrefix        = "N-"
	ParticipantPrefix = "P-"
)

func NewGuid(prefix string) string {
	return prefix + shortuuid.New()
}

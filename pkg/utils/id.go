package utils

import (
	"github.com/lithammer/shortuuid/v3"
)

const (
	RoomPrefix        = "RM_"
	NodePrefix        = "ND_"
	ParticipantPrefix = "PA_"
	TrackPrefix       = "TR_"
	APIKeyPrefix      = "API"
)

func NewGuid(prefix string) string {
	return prefix + shortuuid.New()[:12]
}

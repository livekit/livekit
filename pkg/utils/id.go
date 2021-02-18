package utils

import (
	"crypto/sha1"

	"github.com/lithammer/shortuuid/v3"
	"github.com/lytics/base62"
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

// Creates a hashed ID from a unique string
func HashedID(str string) string {
	h := sha1.New()
	h.Write([]byte(str))
	val := h.Sum(nil)

	return base62.StdEncoding.EncodeToString(val)
}

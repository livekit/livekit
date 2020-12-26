package utils

import (
	"github.com/google/uuid"
	"github.com/lytics/base62"
)

func RandomSecret() string {
	// cannot error
	bin, _ := uuid.New().MarshalBinary()
	return base62.StdEncoding.EncodeToString(bin)
}

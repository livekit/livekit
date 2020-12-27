package utils

import (
	"crypto/rand"
	"io"

	"github.com/lytics/base62"
)

func RandomSecret() string {
	// 256 bit secret
	buf := make([]byte, 32)
	_, err := io.ReadFull(rand.Reader, buf)
	// cannot error
	if err != nil {
		panic("could not read random")
	}
	return base62.StdEncoding.EncodeToString(buf)
}

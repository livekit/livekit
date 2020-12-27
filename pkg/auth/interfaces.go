package auth

import (
	"errors"
	"time"
)

//go:generate go run github.com/maxbrunsfeld/counterfeiter/v6 -generate

var (
	ErrKeysMissing = errors.New("missing API key or secret key")
)

//counterfeiter:generate . TokenIssuer
type TokenIssuer interface {
	CreateToken(claims *GrantClaims, validFor time.Duration) (string, error)
}

//counterfeiter:generate . TokenVerifier
type TokenVerifier interface {
	Verify() (*GrantClaims, error)
}

//counterfeiter:generate . KeyProvider
type KeyProvider interface {
	GetSecret(key string) string
	NumKeys() int
}

package auth

import (
	"errors"
)

//go:generate go run github.com/maxbrunsfeld/counterfeiter/v6 -generate

var (
	ErrKeysMissing = errors.New("missing API key or secret key")
)

////counterfeiter:generate . AccessToken
//type AccessToken interface {
//	SetExpiration(time.Duration) AccessToken
//	Identity(string) AccessToken
//	AddGrant(*VideoGrant) AccessToken
//	ToJWT() (string, error)
//}

//counterfeiter:generate . TokenVerifier
type TokenVerifier interface {
	Identity() string
	Verify(key interface{}) (*VideoGrant, error)
}

//counterfeiter:generate . KeyProvider
type KeyProvider interface {
	GetSecret(key string) string
	NumKeys() int
}

package auth

import (
	"time"

	"gopkg.in/square/go-jose.v2"
	"gopkg.in/square/go-jose.v2/jwt"
)

const (
	defaultValidDuration = 10 * time.Minute
)

// Signer that produces token signed with API key and secret
type AccessToken struct {
	apiKey   string
	secret   string
	identity string
	grant    *VideoGrant
	validFor time.Duration
}

func NewAccessToken(key string, secret string) *AccessToken {
	return &AccessToken{
		apiKey: key,
		secret: secret,
	}
}

func (t *AccessToken) SetIdentity(identity string) *AccessToken {
	t.identity = identity
	return t
}

func (t *AccessToken) SetValidFor(duration time.Duration) *AccessToken {
	t.validFor = duration
	return t
}

func (t *AccessToken) AddGrant(grant *VideoGrant) *AccessToken {
	t.grant = grant
	return t
}

func (t *AccessToken) ToJWT() (string, error) {
	if t.apiKey == "" || t.secret == "" {
		return "", ErrKeysMissing
	}

	sig, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.HS256, Key: []byte(t.secret)},
		(&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		return "", err
	}

	validFor := defaultValidDuration
	if t.validFor > 0 {
		t.validFor = validFor
	}

	cl := jwt.Claims{
		Issuer:    t.apiKey,
		NotBefore: jwt.NewNumericDate(time.Now()),
		Expiry:    jwt.NewNumericDate(time.Now().Add(validFor)),
		ID:        t.identity,
	}
	grants := &ClaimGrants{}
	if t.grant != nil {
		grants.Video = t.grant
	}
	return jwt.Signed(sig).Claims(cl).Claims(grants).CompactSerialize()
}

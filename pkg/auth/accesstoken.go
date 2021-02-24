package auth

import (
	"time"

	"gopkg.in/square/go-jose.v2"
	"gopkg.in/square/go-jose.v2/jwt"
)

const (
	defaultValidDuration = 6 * time.Hour
)

// Signer that produces token signed with API key and secret
type AccessToken struct {
	apiKey     string
	secret     string
	identity   string
	videoGrant *VideoGrant
	metadata   map[string]interface{}
	validFor   time.Duration
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
	t.videoGrant = grant
	return t
}

func (t *AccessToken) SetMetadata(md map[string]interface{}) *AccessToken {
	t.metadata = md
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
		validFor = t.validFor
	}

	cl := jwt.Claims{
		Issuer:    t.apiKey,
		NotBefore: jwt.NewNumericDate(time.Now()),
		Expiry:    jwt.NewNumericDate(time.Now().Add(validFor)),
		ID:        t.identity,
	}
	grants := &ClaimGrants{}
	if t.videoGrant != nil {
		grants.Video = t.videoGrant
	}
	if t.metadata != nil {
		grants.Metadata = t.metadata
	}
	return jwt.Signed(sig).Claims(cl).Claims(grants).CompactSerialize()
}

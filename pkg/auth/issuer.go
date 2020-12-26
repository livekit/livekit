package auth

import (
	"time"

	"gopkg.in/square/go-jose.v2"
	"gopkg.in/square/go-jose.v2/jwt"
)

// Signer that produces token signed with API key and secret
type APIKeyTokenIssuer struct {
	APIKey    string
	SecretKey string
}

func NewAPIKeyTokenIssuer(key string, secret string) *APIKeyTokenIssuer {
	return &APIKeyTokenIssuer{
		APIKey:    key,
		SecretKey: secret,
	}
}

func (s *APIKeyTokenIssuer) CreateToken(claims *GrantClaims, validFor time.Duration) (string, error) {
	if s.APIKey == "" || s.SecretKey == "" {
		return "", ErrKeysMissing
	}
	sig, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.HS256, Key: []byte(s.SecretKey)},
		(&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		return "", err
	}

	cl := jwt.Claims{
		Issuer:    s.APIKey,
		Subject:   s.APIKey,
		NotBefore: jwt.NewNumericDate(time.Now()),
		Expiry:    jwt.NewNumericDate(time.Now().Add(validFor)),
	}

	return jwt.Signed(sig).Claims(cl).Claims(claims).CompactSerialize()
}

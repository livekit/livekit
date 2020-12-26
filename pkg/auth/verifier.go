package auth

import (
	"time"

	"gopkg.in/square/go-jose.v2/jwt"
)

type APIKeyTokenVerifier struct {
	token     *jwt.JSONWebToken
	apiKey    string
	secretKey string
}

func ParseAPIToken(raw string) (*APIKeyTokenVerifier, error) {
	tok, err := jwt.ParseSigned(raw)
	if err != nil {
		return nil, err
	}

	out := jwt.Claims{}
	if err := tok.UnsafeClaimsWithoutVerification(&out); err != nil {
		return nil, err
	}

	return &APIKeyTokenVerifier{
		token:  tok,
		apiKey: out.Issuer,
	}, nil
}

// Returns the API key this token was signed with
func (v *APIKeyTokenVerifier) APIKey() string {
	return v.apiKey
}

func (v *APIKeyTokenVerifier) SetSecretKey(secret string) {
	v.secretKey = secret
}

func (v *APIKeyTokenVerifier) Verify() (*GrantClaims, error) {
	if v.secretKey == "" {
		return nil, ErrKeysMissing
	}
	out := jwt.Claims{}
	claims := GrantClaims{}
	if err := v.token.Claims([]byte(v.secretKey), &out, &claims); err != nil {
		return nil, err
	}
	if err := out.Validate(jwt.Expected{Issuer: v.apiKey, Time: time.Now()}); err != nil {
		return nil, err
	}
	return &claims, nil
}

package service_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/livekit/livekit-server/pkg/auth"
	"github.com/livekit/livekit-server/pkg/auth/authfakes"
	"github.com/livekit/livekit-server/pkg/service"
)

func TestAuthMiddleware(t *testing.T) {
	api := "APIabcdefg"
	secret := "somesecretencodedinbase62"
	provider := &authfakes.FakeKeyProvider{}
	provider.GetSecretReturns(secret)

	m := service.NewAPIKeyAuthMiddleware(provider)
	var grants *auth.ClaimGrants
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		grants = service.GetGrants(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	orig := &auth.VideoGrant{Room: "abcdefg", RoomJoin: true}
	// ensure that the original claim could be retrieved
	at := auth.NewAccessToken(api, secret).
		AddGrant(orig)
	token, err := at.ToJWT()
	assert.NoError(t, err)

	r := &http.Request{Header: http.Header{}}
	w := httptest.NewRecorder()
	service.SetAuthorizationToken(r, token)
	m.ServeHTTP(w, r, handler)

	assert.NotNil(t, grants)
	assert.EqualValues(t, orig, grants.Video)

	// no authorization == no claims
	grants = nil
	w = httptest.NewRecorder()
	r = &http.Request{Header: http.Header{}}
	m.ServeHTTP(w, r, handler)
	assert.Nil(t, grants)
	assert.Equal(t, http.StatusOK, w.Code)

	// incorrect authorization: error
	grants = nil
	w = httptest.NewRecorder()
	r = &http.Request{Header: http.Header{}}
	service.SetAuthorizationToken(r, "invalid token")
	m.ServeHTTP(w, r, handler)
	assert.Nil(t, grants)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

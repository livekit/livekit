// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package service_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/auth/authfakes"

	"github.com/livekit/livekit-server/pkg/service"
)

func TestAuthMiddleware(t *testing.T) {
	api := "APIabcdefg"
	secret := "somesecretencodedinbase62extendto32bytes"
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
	require.NoError(t, err)

	r := &http.Request{Header: http.Header{}}
	w := httptest.NewRecorder()
	service.SetAuthorizationToken(r, token)
	m.ServeHTTP(w, r, handler)

	require.NotNil(t, grants)
	require.EqualValues(t, orig, grants.Video)

	// no authorization == no claims
	grants = nil
	w = httptest.NewRecorder()
	r = &http.Request{Header: http.Header{}}
	m.ServeHTTP(w, r, handler)
	require.Nil(t, grants)
	require.Equal(t, http.StatusOK, w.Code)

	// incorrect authorization: error
	grants = nil
	w = httptest.NewRecorder()
	r = &http.Request{Header: http.Header{}}
	service.SetAuthorizationToken(r, "invalid token")
	m.ServeHTTP(w, r, handler)
	require.Nil(t, grants)
	require.Equal(t, http.StatusUnauthorized, w.Code)
}

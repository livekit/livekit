package auth_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/livekit/livekit-server/pkg/auth"
)

func TestAPIVerifier(t *testing.T) {
	apiKey := "APID3B67uxk4Nj2GKiRPibAZ9"
	secret := "YHC-CUhbQhGeVCaYgn1BNA++"
	accessToken := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJleHAiOjE2MDg5MzAzMDgsImlzcyI6IkFQSUQzQjY3dXhrNE5qMkdLaVJQaWJBWjkiLCJuYmYiOjE2MDg5MjY3MDgsInJvb21fam9pbiI6dHJ1ZSwicm9vbV9zaWQiOiJteWlkIiwic3ViIjoiQVBJRDNCNjd1eGs0TmoyR0tpUlBpYkFaOSJ9.cmHEBq0MLyRqphmVLM2cLXg5ao5Sro7am8yXhcYKcwE"
	t.Run("cannot decode with incorrect key", func(t *testing.T) {
		v, err := auth.ParseAPIToken(accessToken)
		assert.NoError(t, err)

		assert.Equal(t, apiKey, v.APIKey())
		_, err = v.Verify("")
		assert.Error(t, err)

		_, err = v.Verify("anothersecret")
		assert.Error(t, err)
	})

	t.Run("key has expired", func(t *testing.T) {
		v, err := auth.ParseAPIToken(accessToken)

		_, err = v.Verify(secret)
		assert.Error(t, err)
	})

	t.Run("unexpired token is verified", func(t *testing.T) {
		claim := auth.VideoGrant{RoomCreate: true}
		at := auth.NewAccessToken(apiKey, secret).
			AddGrant(&claim).
			SetValidFor(time.Minute).
			SetIdentity("me")
		authToken, err := at.ToJWT()
		assert.NoError(t, err)

		v, err := auth.ParseAPIToken(authToken)
		assert.NoError(t, err)
		assert.Equal(t, apiKey, v.APIKey())
		assert.Equal(t, "me", v.Identity())

		decoded, err := v.Verify(secret)
		assert.NoError(t, err)
		assert.Equal(t, &claim, decoded.Video)
	})
}

package auth_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"gopkg.in/square/go-jose.v2/jwt"

	"github.com/livekit/livekit-server/pkg/auth"
	"github.com/livekit/livekit-server/pkg/utils"
)

func TestAPIIssuer(t *testing.T) {
	t.Run("keys must be set", func(t *testing.T) {
		s := auth.NewAPIKeyTokenIssuer("", "")
		_, err := s.CreateToken(&auth.GrantClaims{}, time.Hour)
		assert.Equal(t, auth.ErrKeysMissing, err)
	})

	t.Run("generates a decodeable key", func(t *testing.T) {
		apiKey, secret := apiKeypair()
		//fmt.Println(apiKey, secret)
		s := auth.NewAPIKeyTokenIssuer(apiKey, secret)
		claims := auth.GrantClaims{RoomJoin: true, Room: "myroom"}
		raw, err := s.CreateToken(&claims, time.Hour)
		//fmt.Println(raw)
		assert.NoError(t, err)

		assert.Len(t, strings.Split(raw, "."), 3)

		// ensure it's a valid JWT
		token, err := jwt.ParseSigned(raw)
		assert.NoError(t, err)

		decodedGrant := auth.GrantClaims{}
		err = token.UnsafeClaimsWithoutVerification(&decodedGrant)
		assert.NoError(t, err)

		assert.EqualValues(t, &claims, &decodedGrant)
	})
}

func apiKeypair() (string, string) {
	return utils.NewGuid(utils.APIKeyPrefix), utils.RandomSecret()
}

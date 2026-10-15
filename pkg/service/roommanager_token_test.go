package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/utils"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/rtc/types/typesfakes"
)

// Exercises refreshToken, the shared function behind every refresh, starting from an encoded join
// token so a grant field the pinned protocol cannot decode is caught too.
func TestRefreshTokenKeepsObservabilityGrant(t *testing.T) {
	for name, obs := range map[string]*auth.ObservabilityGrant{
		"none":              nil,
		"write":             {Write: true},
		"clientWrite":       {ClientWrite: true},
		"write+clientWrite": {Write: true, ClientWrite: true},
	} {
		t.Run(name, func(t *testing.T) {
			secret := utils.RandomSecret()
			r := &RoomManager{config: &config.Config{Keys: map[string]string{"APIkey": secret}}}

			join, err := auth.NewAccessToken("APIkey", secret).
				SetIdentity("alice").
				SetVideoGrant(&auth.VideoGrant{RoomJoin: true, Room: "room"}).
				SetObservabilityGrant(obs).
				SetValidFor(time.Minute).
				ToJWT()
			require.NoError(t, err)

			p := &typesfakes.FakeLocalParticipant{}
			p.IdentityReturns("alice")
			p.ClaimGrantsReturns(verifyToken(t, join, secret))

			require.NoError(t, r.refreshToken(p))
			require.Equal(t, 1, p.SendRefreshTokenCallCount())
			require.Equal(t, obs, verifyToken(t, p.SendRefreshTokenArgsForCall(0), secret).Observability,
				"kept as joined, never added")
		})
	}
}

func verifyToken(t *testing.T, raw, secret string) *auth.ClaimGrants {
	t.Helper()
	v, err := auth.ParseAPIToken(raw)
	require.NoError(t, err)
	_, grants, err := v.Verify(secret)
	require.NoError(t, err)
	return grants
}

package service

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/utils"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/rtc/types/typesfakes"
)

// refreshToken backs every refresh: the first one at join, the periodic tick and claims changes.
func TestRefreshTokenKeepsObservabilityGrant(t *testing.T) {
	for name, obs := range map[string]*auth.ObservabilityGrant{
		"none":  nil,
		"write": {Write: true},
	} {
		t.Run(name, func(t *testing.T) {
			secret := utils.RandomSecret()
			r := &RoomManager{config: &config.Config{Keys: map[string]string{"APIkey": secret}}}

			p := &typesfakes.FakeLocalParticipant{}
			p.IdentityReturns("alice")
			p.ClaimGrantsReturns(&auth.ClaimGrants{
				Identity:      "alice",
				Video:         &auth.VideoGrant{RoomJoin: true, Room: "room"},
				Observability: obs,
			})

			require.NoError(t, r.refreshToken(p))
			require.Equal(t, 1, p.SendRefreshTokenCallCount())

			v, err := auth.ParseAPIToken(p.SendRefreshTokenArgsForCall(0))
			require.NoError(t, err)
			_, grants, err := v.Verify(secret)
			require.NoError(t, err)
			require.Equal(t, obs, grants.Observability, "kept as joined, never added")
		})
	}
}

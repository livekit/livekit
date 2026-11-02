package service

import (
	"net/http"
	"testing"

	"github.com/livekit/protocol/livekit"
	"github.com/stretchr/testify/require"
)

func TestEnsureGatewayAccountMatchesIdentity(t *testing.T) {
	hdr := http.Header{}
	hdr.Set("x-account-id", "acc-1")
	require.NoError(t, ensureGatewayAccountMatchesIdentity(hdr, "user-acc-1", livekit.ParticipantInfo_STANDARD))
}

func TestEnsureGatewayAccountMatchesIdentity_Missing(t *testing.T) {
	require.ErrorIs(t,
		ensureGatewayAccountMatchesIdentity(http.Header{}, "user-acc", livekit.ParticipantInfo_STANDARD),
		ErrMissingGatewayAccountID)
}

func TestEnsureGatewayAccountMatchesIdentity_Mismatch(t *testing.T) {
	hdr := http.Header{}
	hdr.Set("x-account-id", "acc-1")
	require.ErrorIs(t,
		ensureGatewayAccountMatchesIdentity(hdr, "user-other", livekit.ParticipantInfo_STANDARD),
		ErrGatewayAccountMismatch)
}

func TestEnsureGatewayAccountMatchesIdentity_SkipsAgent(t *testing.T) {
	require.NoError(t, ensureGatewayAccountMatchesIdentity(nil, "agent", livekit.ParticipantInfo_AGENT))
}

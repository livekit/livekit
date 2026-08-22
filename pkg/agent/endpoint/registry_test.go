package endpoint

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

// worker ids are stable across reconnects: a re-registration must supersede the
// old epoch, and the retiring connection's Deregister must not strand the new
// one.
func TestRegistrySupersede(t *testing.T) {
	g := NewRegistry()
	manifest, err := ParseManifest([]*livekit.AgentHttp_AgentEndpoint{{
		Path: "/x", Methods: []string{"GET"}, Public: true,
	}})
	require.NoError(t, err)

	mk := func(instance string) *Registration {
		return &Registration{
			WorkerID: "AW_1", InstanceID: instance, APIKey: "key",
			Deployment: "production", Manifest: manifest,
			Settings: Settings{DataConnCount: 1, AttachToken: "ATT_" + instance},
			Logger:   logger.GetLogger(),
		}
	}

	oldReg := mk("i-1")
	require.NoError(t, g.Register(oldReg))
	newReg := mk("i-2")
	require.NoError(t, g.Register(newReg))

	require.Equal(t, []*Registration{newReg}, g.Candidates("key", "production"))
	require.Error(t, g.ValidateAttach("AW_1", "i-1", "key", "ATT_i-1"), "old epoch must not attach")
	require.NoError(t, g.ValidateAttach("AW_1", "i-2", "key", "ATT_i-2"))
	// a grant for another project cannot attach even with the right token
	require.ErrorIs(t, g.ValidateAttach("AW_1", "i-2", "other", "ATT_i-2"), ErrAttachRejected,
		"wrong-project grant must not attach")

	// the old control connection tears down after the new one registered
	g.Deregister(oldReg)
	require.Equal(t, []*Registration{newReg}, g.Candidates("key", "production"),
		"the retiring epoch must not deregister its successor")

	g.Deregister(newReg)
	require.Empty(t, g.Candidates("key", "production"))
}

// Copyright 2026 LiveKit, Inc.

package endpoint

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
)

// fakeSession is a no-op Session for registry tests: it records whether it was
// closed so supersede/deregister can be asserted.
type fakeSession struct {
	closed bool
}

func (s *fakeSession) OpenStream(context.Context) (Stream, error) { return nil, ErrNoSession }
func (s *fakeSession) OpenStreams() int                           { return 0 }
func (s *fakeSession) MaxStreams() int                            { return DefaultMaxStreams }
func (s *fakeSession) Close(string)                               { s.closed = true }

// worker ids are stable across reconnects: a re-registration must supersede the
// old epoch and close its session, and the retiring session's Deregister must
// not strand the new one.
func TestRegistrySupersede(t *testing.T) {
	g := NewRegistry()
	manifest, err := ParseManifest([]*livekit.AgentHttp_AgentEndpoint{{
		Path: "/x", Methods: []string{"GET"}, Public: true,
	}})
	require.NoError(t, err)

	mk := func() (*Registration, *fakeSession) {
		r := &Registration{
			WorkerID: "AW_1", APIKey: "key",
			AgentName: "agent", Deployment: "production", Manifest: manifest,
		}
		s := &fakeSession{}
		r.SetSession(s)
		return r, s
	}

	oldReg, oldSess := mk()
	require.NoError(t, g.Register(oldReg))
	newReg, newSess := mk()
	require.NoError(t, g.Register(newReg))

	require.Equal(t, []*Registration{newReg}, g.Candidates("key", "agent", "production"))
	require.True(t, oldSess.closed, "superseded epoch's session must be closed")
	require.False(t, newSess.closed)

	// the old control connection tears down after the new one registered
	g.Deregister(oldReg)
	require.Equal(t, []*Registration{newReg}, g.Candidates("key", "agent", "production"),
		"the retiring epoch must not deregister its successor")

	g.Deregister(newReg)
	require.Empty(t, g.Candidates("key", "agent", "production"))
	require.True(t, newSess.closed, "deregistered session must be closed")
}

// Candidates keys on (apiKey, agentName, deployment): a request must not see
// another agent's workers, nor another deployment's, in the same project.
func TestRegistryAgentScoping(t *testing.T) {
	g := NewRegistry()
	manifest, err := ParseManifest([]*livekit.AgentHttp_AgentEndpoint{{
		Path: "/x", Methods: []string{"GET"}, Public: true,
	}})
	require.NoError(t, err)

	mk := func(workerID, agentName, deployment string) *Registration {
		r := &Registration{
			WorkerID: workerID, APIKey: "key",
			AgentName: agentName, Deployment: deployment, Manifest: manifest,
		}
		r.SetSession(&fakeSession{})
		return r
	}
	a := mk("AW_a", "alpha", "production")
	b := mk("AW_b", "beta", "production")
	staging := mk("AW_c", "alpha", "staging")
	require.NoError(t, g.Register(a))
	require.NoError(t, g.Register(b))
	require.NoError(t, g.Register(staging))

	require.Equal(t, []*Registration{a}, g.Candidates("key", "alpha", "production"))
	require.Equal(t, []*Registration{b}, g.Candidates("key", "beta", "production"))
	require.Empty(t, g.Candidates("key", "gamma", "production"))
	// a different deployment of the same agent is a separate candidate set
	require.Equal(t, []*Registration{staging}, g.Candidates("key", "alpha", "staging"))
}

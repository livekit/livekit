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
	g, s := NewRegistry(), testScope()
	manifest, err := ParseManifest([]*livekit.AgentHttp_AgentEndpoint{{
		Path: "/x", Methods: []string{"GET"}, Public: true,
	}})
	require.NoError(t, err)

	mk := func() (*Registration, *fakeSession) {
		sess := &fakeSession{}
		return NewRegistration(RegistrationParams{
			WorkerID: "AW_1", Manifest: manifest, Session: sess,
		}), sess
	}

	oldReg, oldSess := mk()
	g.Register(s, oldReg)
	newReg, newSess := mk()
	g.Register(s, newReg)

	require.Equal(t, []*Registration{newReg}, s.Candidates())
	require.True(t, oldSess.closed, "superseded epoch's session must be closed")
	require.False(t, newSess.closed)

	// the old control connection tears down after the new one registered
	g.Deregister(oldReg)
	require.Equal(t, []*Registration{newReg}, s.Candidates(),
		"the retiring epoch must not deregister its successor")

	g.Deregister(newReg)
	require.Empty(t, s.Candidates())
	require.True(t, s.Empty())
	require.True(t, newSess.closed, "deregistered session must be closed")
}

// The fence is node-wide and scope-blind: worker ids are server-issued, so an
// epoch is superseded wherever it was scoped, and scopes are otherwise
// independent.
func TestRegistryScopesAreIndependent(t *testing.T) {
	g := NewRegistry()
	alpha, beta := testScope(), testScope()
	m, err := ParseManifest([]*livekit.AgentHttp_AgentEndpoint{{
		Path: "/x", Methods: []string{"GET"}, Public: true,
	}})
	require.NoError(t, err)

	a := NewRegistration(RegistrationParams{WorkerID: "AW_a", Manifest: m, Session: &fakeSession{}})
	b := NewRegistration(RegistrationParams{WorkerID: "AW_b", Manifest: m, Session: &fakeSession{}})
	g.Register(alpha, a)
	g.Register(beta, b)

	require.Equal(t, []*Registration{a}, alpha.Candidates())
	require.Equal(t, []*Registration{b}, beta.Candidates())

	// a scope nothing holds is empty, and holds no route table at all
	g.Deregister(a)
	require.True(t, alpha.Empty())
	require.Nil(t, alpha.routeTable())
	require.Equal(t, []*Registration{b}, beta.Candidates(), "one scope draining leaves the other")
}

// A nil scope is the "no worker here holds this deployment" answer, and must be
// safe for the front to interrogate without a separate test.
func TestNilScopeIsEmpty(t *testing.T) {
	var s *Scope
	require.Nil(t, s.Candidates())
	require.Nil(t, s.routeTable())
	require.True(t, s.Empty())
}

package endpoint

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/psrpc"
)

func TestRemoteResolve(t *testing.T) {
	bus := psrpc.NewLocalMessageBus()
	log := logger.GetLogger()

	mkNode := func(nodeID string) (*Registry, *Remote) {
		reg := NewRegistry()
		rem, err := NewRemote(reg, bus, nodeID, "http://"+nodeID, log)
		require.NoError(t, err)
		t.Cleanup(rem.Close)
		return reg, rem
	}
	regA, _ := mkNode("node-a")
	regB, remB := mkNode("node-b")

	manifest, err := ParseManifest([]*livekit.AgentHttp_AgentEndpoint{{
		Path: "/sms", Methods: []string{"POST"}, Public: true,
	}})
	require.NoError(t, err)

	mk := func(reg *Registry, id string) *Registration {
		r := &Registration{
			WorkerID: id, InstanceID: "i-" + id, APIKey: "test",
			Deployment: "production", Manifest: manifest,
			Settings: Settings{DataConnCount: 1, AttachToken: "ATT"},
			Logger:   log,
		}
		require.NoError(t, reg.Register(r))
		return r
	}
	ra := mk(regA, "wa")
	rb := mk(regB, "wb")
	for _, r := range []*Registration{ra, rb} {
		r.mu.Lock()
		r.conns = append(r.conns, &DataConn{})
		r.mu.Unlock()
	}

	// capacity picks a node; either node is valid
	resp, err := remB.Resolve(context.Background(), &rpc.ResolveEndpointRequest{
		Scope: "test", Deployment: "production", Path: "/sms", Method: "POST",
		Authenticated: false,
	})
	require.NoError(t, err)
	require.Contains(t, []string{"node-a", "node-b"}, resp.NodeId)

	// typed misses: unknown path resolves NotFound, wrong method FailedPrecondition
	_, err = remB.Resolve(context.Background(), &rpc.ResolveEndpointRequest{
		Scope: "test", Deployment: "production", Path: "/nope", Method: "GET",
	})
	var perr psrpc.Error
	require.ErrorAs(t, err, &perr)
	require.Equal(t, psrpc.NotFound, perr.Code())

	_, err = remB.Resolve(context.Background(), &rpc.ResolveEndpointRequest{
		Scope: "test", Deployment: "production", Path: "/sms", Method: "GET",
	})
	require.ErrorAs(t, err, &perr)
	require.Equal(t, psrpc.FailedPrecondition, perr.Code())
}

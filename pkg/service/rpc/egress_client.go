package rpc

import (
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/psrpc"
)

type EgressClient interface {
	EgressInternalClient
	EgressHandlerClient
}

type egressClient struct {
	EgressInternalClient
	EgressHandlerClient
}

func NewEgressClient(nodeID livekit.NodeID, bus psrpc.MessageBus) (EgressClient, error) {
	if bus == nil {
		return nil, nil
	}

	clientID := string(nodeID)
	internalClient, err := NewEgressInternalClient(clientID, bus)
	if err != nil {
		return nil, err
	}
	handlerClient, err := NewEgressHandlerClient(clientID, bus)
	if err != nil {
		return nil, err
	}
	return &egressClient{
		EgressInternalClient: internalClient,
		EgressHandlerClient:  handlerClient,
	}, nil
}

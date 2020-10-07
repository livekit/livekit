//+build wireinject

package main

import (
	"github.com/google/wire"
	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/node"
	"github.com/livekit/livekit-server/pkg/service"
)

func InitializeServer(conf *config.Config) (*LivekitServer, error) {
	wire.Build(
		NewLivekitServer,
		node.NodeSet,
		service.ServiceSet,
	)
	return &LivekitServer{}, nil
}

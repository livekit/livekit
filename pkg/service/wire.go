//+build wireinject

package service

import (
	"github.com/google/wire"

	"github.com/livekit/livekit-server/pkg/auth"
	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/node"
)

func InitializeServer(conf *config.Config, keyProvider auth.KeyProvider) (*LivekitServer, error) {
	wire.Build(
		node.NodeSet,
		ServiceSet,
	)
	return &LivekitServer{}, nil
}

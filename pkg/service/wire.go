//+build wireinject

package service

import (
	"github.com/google/wire"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
)

func InitializeServer(conf *config.Config, currentNode routing.LocalNode) (*LivekitServer, error) {
	wire.Build(
		ServiceSet,
	)
	return &LivekitServer{}, nil
}

func InitializeRouter(conf *config.Config, currentNode routing.LocalNode) (routing.Router, error) {
	wire.Build(
		wire.NewSet(
			createRedisClient,
			createRouter,
		),
	)

	return nil, nil
}
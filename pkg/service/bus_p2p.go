package service

import (
	"context"
	p2p_database "github.com/dTelecom/p2p-realtime-database"

	"google.golang.org/protobuf/proto"
)

type P2pBus struct {
	db *p2p_database.DB
}

func (p P2pBus) Publish(ctx context.Context, channel string, msg proto.Message) error {
	//TODO implement me
	panic("implement me")
}

func (p P2pBus) Subscribe(ctx context.Context, channel string, channelSize int) (interface{}, error) {
	//TODO implement me
	panic("implement me")
}

func (p P2pBus) SubscribeQueue(ctx context.Context, channel string, channelSize int) (interface{}, error) {
	//TODO implement me
	panic("implement me")
}

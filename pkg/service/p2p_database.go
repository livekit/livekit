package service

import (
	"context"
	p2p_database "github.com/dTelecom/p2p-realtime-database"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
)

type P2PDatabase interface {
	Subscribe(ctx context.Context, topic string, handler p2p_database.PubSubHandler, opts ...pubsub.TopicOpt) error
	Publish(ctx context.Context, topic string, value interface{}, opts ...pubsub.PubOpt) (p2p_database.Event, error)
}

type MainP2PDatabase interface {
	P2PDatabase
}

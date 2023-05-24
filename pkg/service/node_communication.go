package service

import (
	"context"
	p2p_database "github.com/dTelecom/p2p-realtime-database"
	"github.com/pkg/errors"
)

const (
	prefixIncomingMessageTopic = "incoming_messages_"
	defaultBufferMessagesChan  = 100
)

type NodeCommunication struct {
	messages chan p2p_database.Event
	db       *p2p_database.DB
}

func NewNodeCommunication(db *p2p_database.DB) *NodeCommunication {
	return &NodeCommunication{
		messages: make(chan p2p_database.Event, defaultBufferMessagesChan),
		db:       db,
	}
}

func (c *NodeCommunication) SendAsyncMessageToPeerId(ctx context.Context, peerId string, message interface{}) (string, error) {
	m, err := c.db.Publish(ctx, c.messagesP2PTopicName(peerId), message)
	if err != nil {
		return "", errors.Wrap(err, "db publish")
	}
	return m.ID, nil
}

func (c *NodeCommunication) ListenIncomingMessages(ctx context.Context) (chan p2p_database.Event, error) {
	err := c.db.Subscribe(ctx, c.messagesP2PTopicName(c.db.GetHost().ID().String()), func(event p2p_database.Event) {
		c.messages <- event
	})
	if err != nil {
		return nil, errors.Wrap(err, "subscribe message topic")
	}
	return c.messages, nil
}

func (c *NodeCommunication) GetLocalPeedID() string {
	return c.db.GetHost().ID().String()
}

func (c *NodeCommunication) messagesP2PTopicName(peerId string) string {
	return prefixIncomingMessageTopic + peerId
}

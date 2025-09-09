package p2p

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.uber.org/atomic"

	"github.com/dTelecom/p2p-database/pubsub"

	p2p_common "github.com/dTelecom/p2p-database/common"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/pkg/errors"
)

const (
	roomMessagesTopicFmt     = "room_messages_%v"
	incomingMessagesTopicFmt = "incoming_messages_%v_%v"
	pingMessage              = "ping"
	pongMessage              = "pong"
	adMessage                = "ad"
)

type RoomCommunicatorImpl struct {
	key string

	ctx    context.Context
	cancel context.CancelFunc
	closed atomic.Bool

	mainDatabase *pubsub.DB

	peers           map[string]struct{}
	peerHandlers    []func(peerId string)
	messageHandlers []func(message interface{}, fromPeerId string, eventId string)

	mu sync.Mutex
	logger logger.Logger
}

func NewRoomCommunicatorImpl(key livekit.RoomKey, mainDatabase *pubsub.DB) (*RoomCommunicatorImpl, error) {
	ctx, cancel := context.WithCancel(context.Background())

	roomCommunicator := &RoomCommunicatorImpl{
		key:          string(key),
		ctx:          ctx,
		cancel:       cancel,
		mainDatabase: mainDatabase,
		peers:        make(map[string]struct{}),
		logger:       logger.GetLogger(),
	}

	err := roomCommunicator.init()

	return roomCommunicator, err
}

func (c *RoomCommunicatorImpl) Close() {
	c.closed.Store(true)

	c.cancel()

	ctx1, cancel1 := context.WithTimeout(context.Background(), time.Second*15)
	defer cancel1()

	incomingMessagesTopic := formatIncomingMessagesTopic(c.key, c.mainDatabase.GetHost().ID().String())
	err := c.mainDatabase.Unsubscribe(ctx1, incomingMessagesTopic)
	if err != nil {
		c.logger.Errorw("Failed to unsubscribe from topic", err, "topic", incomingMessagesTopic, "roomKey", c.key)
	} else {
		c.logger.Debugw("Unsubscribed from incoming messages topic", "topic", incomingMessagesTopic, "roomKey", c.key)
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second*15)
	defer cancel2()

	roomMessagesTopic := formatRoomMessageTopic(c.key)
	err = c.mainDatabase.Unsubscribe(ctx2, roomMessagesTopic)
	if err != nil {
		c.logger.Errorw("Failed to unsubscribe from topic", err, "topic", roomMessagesTopic, "roomKey", c.key)
	} else {
		c.logger.Debugw("Unsubscribed from room messages topic", "topic", roomMessagesTopic, "roomKey", c.key)
	}
}

func (c *RoomCommunicatorImpl) init() error {

	incomingMessagesTopic := formatIncomingMessagesTopic(c.key, c.mainDatabase.GetHost().ID().String())
	if err := c.mainDatabase.Subscribe(context.Background(), incomingMessagesTopic, c.incomingMessageHandler); err != nil {
		return errors.Wrap(err, "cannot subscribe to incoming messages topic")
	}
	c.logger.Debugw("Subscribed to incoming messages topic", "topic", incomingMessagesTopic, "roomKey", c.key)

	roomMessagesTopic := formatRoomMessageTopic(c.key)
	if err := c.mainDatabase.Subscribe(context.Background(), roomMessagesTopic, c.roomMessageHandler); err != nil {
		return errors.Wrap(err, "cannot subscribe to room messages topic")
	}
	c.logger.Debugw("Subscribed to room messages topic", "topic", roomMessagesTopic, "roomKey", c.key)

	go func() {
		for {
			c.publishAd(roomMessagesTopic)
			select {
			case <-c.ctx.Done():
				return
			case <-time.After(5 * time.Second):
				continue
			}
		}
	}()

	return nil
}

func (c *RoomCommunicatorImpl) publishAd(roomMessagesTopic string) {
	if c.closed.Load() {
		c.logger.Debugw("Room communicator closed, skipping publish", "roomKey", c.key)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*15)
	defer cancel()

	if _, err := c.mainDatabase.Publish(ctx, roomMessagesTopic, adMessage); err != nil {
		c.logger.Errorw("Failed to publish ad message", err, "topic", roomMessagesTopic, "roomKey", c.key)
	} else {
		c.logger.Debugw("Published ad message", "topic", roomMessagesTopic, "roomKey", c.key)
	}
}

func (c *RoomCommunicatorImpl) checkPeer(peerId string) {
	c.mu.Lock()
	if _, ok := c.peers[peerId]; ok {
		c.mu.Unlock()
	} else {
		c.logger.Infow("Discovered new peer", "peerId", peerId, "roomKey", c.key)
		c.peers[peerId] = struct{}{}
		for _, peerHandler := range c.peerHandlers {
			go peerHandler(peerId)
		}
		c.mu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second*15)
		defer cancel()

		if c.closed.Load() {
			c.logger.Debugw("Room communicator closed", "roomKey", c.key)
			return
		}

		incomingMessageTopic := formatIncomingMessagesTopic(c.key, peerId)
		if _, err := c.mainDatabase.Publish(ctx, incomingMessageTopic, pingMessage); err != nil {
			c.logger.Errorw("Failed to send ping message", err, "peerId", peerId, "roomKey", c.key)
		} else {
			c.logger.Debugw("Ping message sent", "peerId", peerId, "roomKey", c.key)
		}
	}
}

func (c *RoomCommunicatorImpl) incomingMessageHandler(event p2p_common.Event) {

	if event.FromPeerId == c.mainDatabase.GetHost().ID().String() {
		return
	}

	if event.Message == pingMessage {
		c.logger.Debugw("Ping message received", "fromPeerId", event.FromPeerId, "roomKey", c.key)

		if c.closed.Load() {
			c.logger.Debugw("Room communicator closed", "roomKey", c.key)
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), time.Second*15)
		defer cancel()

		incomingMessageTopic := formatIncomingMessagesTopic(c.key, event.FromPeerId)
		if _, err := c.mainDatabase.Publish(ctx, incomingMessageTopic, pongMessage); err != nil {
			c.logger.Errorw("Failed to send pong message", err, "peerId", event.FromPeerId, "roomKey", c.key)
		} else {
			c.logger.Debugw("Pong message sent", "peerId", event.FromPeerId, "roomKey", c.key)
		}
	} else if event.Message == pongMessage {
		c.logger.Debugw("Pong message received", "fromPeerId", event.FromPeerId, "roomKey", c.key)
	} else if event.Message == adMessage {
		c.checkPeer(event.FromPeerId)
	} else {
		c.mu.Lock()
		for _, messageHandler := range c.messageHandlers {
			go messageHandler(event.Message, event.FromPeerId, event.ID)
		}
		c.mu.Unlock()
	}
}

func (c *RoomCommunicatorImpl) roomMessageHandler(event p2p_common.Event) {

	if event.FromPeerId == c.mainDatabase.GetHost().ID().String() {
		return
	}

	if event.Message == adMessage {
		c.checkPeer(event.FromPeerId)

		ctx, cancel := context.WithTimeout(context.Background(), time.Second*15)
		defer cancel()

		if c.closed.Load() {
			c.logger.Debugw("Room communicator closed", "roomKey", c.key)
			return
		}

		incomingMessageTopic := formatIncomingMessagesTopic(c.key, event.FromPeerId)
		if _, err := c.mainDatabase.Publish(ctx, incomingMessageTopic, adMessage); err != nil {
			c.logger.Errorw("Failed to send ad message", err, "peerId", event.FromPeerId, "roomKey", c.key)
		} else {
			c.logger.Debugw("Ad message sent", "peerId", event.FromPeerId, "roomKey", c.key)
		}
	} else {
		c.logger.Warnw("Unknown room message received", nil, "fromPeerId", event.FromPeerId, "roomKey", c.key)
	}
}

func (c *RoomCommunicatorImpl) ForEachPeer(peerHandler func(peerId string)) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.peerHandlers = append(c.peerHandlers, peerHandler)
	for peer := range c.peers {
		peerHandler(peer)
	}
}

func (c *RoomCommunicatorImpl) SendMessage(peerId string, message interface{}) (string, error) {

	if c.closed.Load() {
		return "", fmt.Errorf("room communicator closed for room %v", c.key)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*15)
	defer cancel()

	incomingMessagesTopic := formatIncomingMessagesTopic(c.key, peerId)
	event, err := c.mainDatabase.Publish(ctx, incomingMessagesTopic, message)
	if err != nil {
		return "", errors.Wrap(err, "publish error")
	}
	return event.ID, nil
}

func (c *RoomCommunicatorImpl) OnMessage(messageHandler func(message interface{}, fromPeerId string, eventId string)) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.messageHandlers = append(c.messageHandlers, messageHandler)
}

func formatRoomMessageTopic(roomKey string) string {
	return fmt.Sprintf(roomMessagesTopicFmt, roomKey)
}

func formatIncomingMessagesTopic(roomKey, peerId string) string {
	return fmt.Sprintf(incomingMessagesTopicFmt, roomKey, peerId)
}

package p2p

import (
	"context"
	"fmt"
	"go.uber.org/atomic"
	"sync"
	"time"

	"github.com/dTelecom/p2p-database/pubsub"
	"github.com/livekit/protocol/logger"

	p2p_common "github.com/dTelecom/p2p-database/common"
	"github.com/livekit/protocol/livekit"
	"github.com/pkg/errors"
)

const (
	roomMessagesTopicFmt = "room_messages_%v"
)

type roomCommunicatorMessage struct {
	To      string      `json:"to"`
	Type    string      `json:"type"`
	Payload interface{} `json:"payload"`
}

type RoomCommunicatorImpl struct {
	key string

	ctx    context.Context
	cancel context.CancelFunc
	closed atomic.Bool

	mainDatabase *pubsub.DB
	logger       logger.Logger

	peers           map[string]struct{}
	peerHandlers    []func(peerId string)
	messageHandlers []func(message interface{}, fromPeerId string, eventId string)

	mu sync.Mutex
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

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*15)
	defer cancel()

	roomMessagesTopic := formatRoomMessageTopic(c.key)
	err := c.mainDatabase.Unsubscribe(ctx, roomMessagesTopic)
	if err != nil {
		c.logger.Errorw("Failed to unsubscribe from topic", err, "topic", roomMessagesTopic, "roomKey", c.key)
	} else {
		c.logger.Debugw("Unsubscribed from room messages topic", "topic", roomMessagesTopic, "roomKey", c.key)
	}
}

func (c *RoomCommunicatorImpl) init() error {

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
		c.logger.Debugw("Room communicator closed, skipping publish ad", "roomKey", c.key)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*15)
	defer cancel()

	data := &roomCommunicatorMessage{
		To:      "",
		Type:    "ad",
		Payload: nil,
	}

	if _, err := c.mainDatabase.Publish(ctx, roomMessagesTopic, data); err != nil {
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
	}
}

func (c *RoomCommunicatorImpl) roomMessageHandler(event p2p_common.Event) {

	if event.FromPeerId == c.mainDatabase.GetHost().ID().String() {
		return
	}

	c.logger.Debugw("Received room message", "message", event.Message, "roomKey", c.key)

	messageMap, ok := event.Message.(map[string]interface{})
	if !ok {
		c.logger.Errorw("Failed to cast message", fmt.Errorf("event.Message %v", event.Message), "roomKey", c.key)
		return
	}

	typeValue, ok := messageMap["type"]
	if !ok {
		c.logger.Errorw("Failed to cast message", fmt.Errorf("event.Message %v", event.Message), "roomKey", c.key)
		return
	}

	messageType, ok := typeValue.(string)
	if !ok {
		c.logger.Errorw("Failed to cast message", fmt.Errorf("event.Message %v", event.Message), "roomKey", c.key)
		return
	}

	if messageType == "ad" {
		c.checkPeer(event.FromPeerId)
	} else {
		toValue, ok := messageMap["to"]
		if !ok {
			c.logger.Errorw("Failed to cast message", fmt.Errorf("event.Message %v", event.Message), "roomKey", c.key)
			return
		}

		to, ok := toValue.(string)
		if !ok {
			c.logger.Errorw("Failed to cast message", fmt.Errorf("event.Message %v", event.Message), "roomKey", c.key)
			return
		}

		if to != c.mainDatabase.GetHost().ID().String() {
			return
		}

		c.mu.Lock()
		for _, messageHandler := range c.messageHandlers {
			go messageHandler(messageMap["payload"], event.FromPeerId, event.ID)
		}
		c.mu.Unlock()
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

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()

	data := &roomCommunicatorMessage{
		To:      peerId,
		Type:    "signal",
		Payload: message,
	}

	roomMessagesTopic := formatRoomMessageTopic(c.key)
	event, err := c.mainDatabase.Publish(ctx, roomMessagesTopic, data)

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

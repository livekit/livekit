package p2p

import (
	"context"
	"fmt"
	"sync"
	"time"

	pubsub "github.com/dTelecom/pubsub-solana"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/pkg/errors"
)

const (
	roomMessagesTopicFmt     = "room_messages_%v"
	incomingMessagesTopicFmt = "incoming_messages_%v_%v"
	adMessage                = "ad"
)

type RoomCommunicatorImpl struct {
	room *livekit.Room

	ctx    context.Context
	cancel context.CancelFunc

	pubSub *pubsub.PubSub

	peers           map[string]struct{}
	peerHandlers    []func(peerId string)
	messageHandlers []func(message []byte, fromPeerId string, eventId string)

	mu sync.Mutex
}

func NewRoomCommunicatorImpl(room *livekit.Room, pubSub *pubsub.PubSub) (*RoomCommunicatorImpl, error) {
	ctx, cancel := context.WithCancel(context.Background())

	roomCommunicator := &RoomCommunicatorImpl{
		room:   room,
		ctx:    ctx,
		cancel: cancel,
		pubSub: pubSub,
		peers:  make(map[string]struct{}),
	}

	if err := roomCommunicator.init(); err != nil {
		return nil, errors.Wrap(err, "cannot init room communicator")
	}

	return roomCommunicator, nil
}

func (c *RoomCommunicatorImpl) Close() {
	c.cancel()
}

func (c *RoomCommunicatorImpl) init() error {
	incomingMessagesTopic := formatIncomingMessagesTopic(c.room.Key, c.pubSub.GetPeerId())
	c.pubSub.Subscribe(incomingMessagesTopic, c.incomingMessageHandler)
	logger.Debugw("subscribed to", "topic", incomingMessagesTopic)

	roomMessagesTopic := formatRoomMessageTopic(c.room.Key)
	c.pubSub.Subscribe(roomMessagesTopic, c.roomMessageHandler)
	logger.Debugw("subscribed to", "topic", roomMessagesTopic)

	go func() {
		for {
			if _, err := c.pubSub.Publish(roomMessagesTopic, []byte(adMessage)); err != nil {
				if c.ctx.Err() == nil {
					logger.Errorw("cannot publish ad message", err)
				} else {
					return
				}
			}
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

func (c *RoomCommunicatorImpl) checkPeer(peerId string) {
	c.mu.Lock()
	if _, ok := c.peers[peerId]; ok {
		c.mu.Unlock()
	} else {
		logger.Debugw("New key added peer not exist", "peer", peerId)
		c.peers[peerId] = struct{}{}
		for _, peerHandler := range c.peerHandlers {
			go peerHandler(peerId)
		}
		c.mu.Unlock()
		logger.Debugw("New key added peer added", "peer", peerId)
	}
}

func (c *RoomCommunicatorImpl) incomingMessageHandler(_ context.Context, event pubsub.Event) {
	if string(event.Message) == adMessage {
		c.checkPeer(event.FromPeerId)
	} else {
		c.mu.Lock()
		for _, messageHandler := range c.messageHandlers {
			go messageHandler(event.Message, event.FromPeerId, event.ID)
		}
		c.mu.Unlock()
	}
}

func (c *RoomCommunicatorImpl) roomMessageHandler(_ context.Context, event pubsub.Event) {
	if string(event.Message) == adMessage {
		c.checkPeer(event.FromPeerId)
	} else {
		logger.Errorw("unknown room message from peer", fmt.Errorf(event.FromPeerId))
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

func (c *RoomCommunicatorImpl) SendMessage(peerId string, message []byte) (string, error) {
	incomingMessagesTopic := formatIncomingMessagesTopic(c.room.Key, peerId)
	eventID, err := c.pubSub.Publish(incomingMessagesTopic, message)
	if err != nil {
		return "", errors.Wrap(err, "publish error")
	}
	return eventID, nil
}

func (c *RoomCommunicatorImpl) OnMessage(messageHandler func(message []byte, fromPeerId string, eventId string)) {
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

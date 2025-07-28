package p2p

import (
	"context"
	"fmt"
	"go.uber.org/atomic"
	"log"
	"sync"
	"time"

	"github.com/dTelecom/p2p-database/pubsub"

	p2p_common "github.com/dTelecom/p2p-database/common"
	"github.com/livekit/protocol/livekit"
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
}

func NewRoomCommunicatorImpl(key livekit.RoomKey, mainDatabase *pubsub.DB) (*RoomCommunicatorImpl, error) {
	ctx, cancel := context.WithCancel(context.Background())

	roomCommunicator := &RoomCommunicatorImpl{
		key:          string(key),
		ctx:          ctx,
		cancel:       cancel,
		mainDatabase: mainDatabase,
		peers:        make(map[string]struct{}),
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
		log.Printf("unsubscrib from topic err %v %v", incomingMessagesTopic, err)
	} else {
		log.Printf("unsubscribed from topic %v", incomingMessagesTopic)
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second*15)
	defer cancel2()

	roomMessagesTopic := formatRoomMessageTopic(c.key)
	err = c.mainDatabase.Unsubscribe(ctx2, roomMessagesTopic)
	if err != nil {
		log.Printf("unsubscrib from topic err %v %v", roomMessagesTopic, err)
	} else {
		log.Printf("unsubscribed from topic %v", roomMessagesTopic)
	}
}

func (c *RoomCommunicatorImpl) init() error {

	incomingMessagesTopic := formatIncomingMessagesTopic(c.key, c.mainDatabase.GetHost().ID().String())
	if err := c.mainDatabase.Subscribe(context.Background(), incomingMessagesTopic, c.incomingMessageHandler); err != nil {
		return errors.Wrap(err, "cannot subscribe to incoming messages topic")
	}
	log.Printf("subscribed to topic %v", incomingMessagesTopic)

	roomMessagesTopic := formatRoomMessageTopic(c.key)
	if err := c.mainDatabase.Subscribe(context.Background(), roomMessagesTopic, c.roomMessageHandler); err != nil {
		return errors.Wrap(err, "cannot subscribe to room messages topic")
	}
	log.Printf("subscribed to topic %v", roomMessagesTopic)

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
		log.Printf("RoomCommunicatorImpl closed %v", c.key)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*15)
	defer cancel()

	if _, err := c.mainDatabase.Publish(ctx, roomMessagesTopic, adMessage); err != nil {
		log.Printf("cannot publish ad message: %v", err)
	}
}

func (c *RoomCommunicatorImpl) checkPeer(peerId string) {
	c.mu.Lock()
	if _, ok := c.peers[peerId]; ok {
		c.mu.Unlock()
	} else {
		log.Printf("New key added peer not exist %v", peerId)
		c.peers[peerId] = struct{}{}
		for _, peerHandler := range c.peerHandlers {
			go peerHandler(peerId)
		}
		c.mu.Unlock()
		log.Printf("New key added peer added %v", peerId)

		ctx, cancel := context.WithTimeout(context.Background(), time.Second*15)
		defer cancel()

		if c.closed.Load() {
			log.Printf("RoomCommunicatorImpl closed %v", c.key)
			return
		}

		incomingMessageTopic := formatIncomingMessagesTopic(c.key, peerId)
		if _, err := c.mainDatabase.Publish(ctx, incomingMessageTopic, pingMessage); err != nil {
			log.Printf("cannot send ping message for node %s in db %s: %s", peerId, c.key, err)
		} else {
			log.Printf("PING message sent to %v", peerId)
		}
	}
}

func (c *RoomCommunicatorImpl) incomingMessageHandler(event p2p_common.Event) {

	if event.FromPeerId == c.mainDatabase.GetHost().ID().String() {
		return
	}

	if event.Message == pingMessage {
		log.Println("PING message received")

		if c.closed.Load() {
			log.Printf("RoomCommunicatorImpl closed %v", c.key)
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), time.Second*15)
		defer cancel()

		incomingMessageTopic := formatIncomingMessagesTopic(c.key, event.FromPeerId)
		if _, err := c.mainDatabase.Publish(ctx, incomingMessageTopic, pongMessage); err != nil {
			log.Printf("cannot send pong message for node %s in db %s: %s", event.FromPeerId, c.key, err)
		} else {
			log.Printf("PONG message sent to %v", event.FromPeerId)
		}
	} else if event.Message == pongMessage {
		log.Printf("PONG message received from %v", event.FromPeerId)
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
			log.Printf("RoomCommunicatorImpl closed %v", c.key)
			return
		}

		incomingMessageTopic := formatIncomingMessagesTopic(c.key, event.FromPeerId)
		if _, err := c.mainDatabase.Publish(ctx, incomingMessageTopic, adMessage); err != nil {
			log.Printf("cannot send ad message for node %s in db %s: %s", event.FromPeerId, c.key, err)
		} else {
			log.Printf("ad message sent to %v", event.FromPeerId)
		}
	} else {
		log.Printf("unknown room message from peer: %v", event.FromPeerId)
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
		return "", fmt.Errorf("RoomCommunicatorImpl closed %v", c.key)
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

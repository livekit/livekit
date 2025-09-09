package p2p

import (
	"context"
	"encoding/base64"
	"errors"
	"time"

	p2p_common "github.com/dTelecom/p2p-database/common"
	"github.com/dTelecom/p2p-database/pubsub"
	"go.uber.org/atomic"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"google.golang.org/protobuf/proto"
)

type routerMessage struct {
	Data string `json:"data"`
}

func packRouterMessage(data []byte) interface{} {
	return &routerMessage{
		Data: base64.StdEncoding.EncodeToString(data),
	}
}

func unpackRouterMessage(message interface{}) (data []byte, err error) {
	messageMap, ok := message.(map[string]interface{})
	if !ok {
		err = errors.New("cannot cast")
		return
	}

	dataBase64Value, ok := messageMap["data"]
	if !ok {
		err = errors.New("data undefined")
		return
	}
	dataBase64, ok := dataBase64Value.(string)
	if !ok {
		err = errors.New("cannot cast data to string")
		return
	}
	data, err = base64.StdEncoding.DecodeString(dataBase64)

	return
}

type RouterCommunicatorImpl struct {
	topic          string
	key            livekit.RoomKey
	mainDatabase   *pubsub.DB
	closed         atomic.Bool
	messageHandler func(ctx context.Context, roomKey livekit.RoomKey, msg *livekit.RTCNodeMessage) error
	logger         logger.Logger
}

func NewRouterCommunicatorImpl(key livekit.RoomKey, mainDatabase *pubsub.DB, messageHandler func(ctx context.Context, roomKey livekit.RoomKey, msg *livekit.RTCNodeMessage) error) (*RouterCommunicatorImpl, error) {

	topic := "livekit_router_" + string(key)

	routerCommunicator := &RouterCommunicatorImpl{
		topic:          topic,
		key:            key,
		mainDatabase:   mainDatabase,
		messageHandler: messageHandler,
		logger:         logger.GetLogger(),
	}

	err := routerCommunicator.init()

	return routerCommunicator, err
}

func (c *RouterCommunicatorImpl) Close() {
	c.closed.Store(true)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*15)
	defer cancel()

	err := c.mainDatabase.Unsubscribe(ctx, c.topic)
	if err != nil {
		c.logger.Errorw("Failed to unsubscribe from topic", err, "topic", c.topic, "roomKey", c.key)
	} else {
		c.logger.Debugw("Unsubscribed from topic", "topic", c.topic, "roomKey", c.key)
	}
}

func (c *RouterCommunicatorImpl) Publish(message *livekit.RTCNodeMessage) {
	if c.closed.Load() {
		c.logger.Debugw("Router communicator closed, skipping publish", "roomKey", c.key)
		return
	}

	data, err := proto.Marshal((*livekit.RTCNodeMessage)(message))
	if err != nil {
		c.logger.Errorw("Failed to marshal RTCNodeMessage", err, "roomKey", c.key)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*15)
	defer cancel()

	if _, err := c.mainDatabase.Publish(ctx, c.topic, packRouterMessage(data)); err != nil {
		c.logger.Errorw("Failed to publish router message", err, "topic", c.topic, "roomKey", c.key)
	} else {
		c.logger.Debugw("Published router message", "topic", c.topic, "roomKey", c.key)
	}
}

func (c *RouterCommunicatorImpl) init() error {

	err := c.mainDatabase.Subscribe(context.Background(), c.topic, c.dbHandler)
	return err
}

func (c *RouterCommunicatorImpl) dbHandler(event p2p_common.Event) {

	if event.FromPeerId == c.mainDatabase.GetHost().ID().String() {
		return
	}

	data, err := unpackRouterMessage(event.Message)
	if err != nil {
		c.logger.Errorw("Failed to unpack router message", err, "fromPeerId", event.FromPeerId, "roomKey", c.key)
		return
	}

	rm := livekit.RTCNodeMessage{}
	if err := proto.Unmarshal(data, &rm); err != nil {
		c.logger.Errorw("Failed to unmarshal RTCNodeMessage", err, "fromPeerId", event.FromPeerId, "roomKey", c.key)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*15)
	defer cancel()

	err = c.messageHandler(ctx, c.key, &rm)
	if err != nil {
		c.logger.Errorw("Message handler error", err, "fromPeerId", event.FromPeerId, "roomKey", c.key)
	} else {
		c.logger.Debugw("Processed router message", "fromPeerId", event.FromPeerId, "roomKey", c.key)
	}
}

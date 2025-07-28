package p2p

import (
	"context"
	"encoding/base64"
	"errors"
	p2p_common "github.com/dTelecom/p2p-database/common"
	"github.com/dTelecom/p2p-database/pubsub"
	"go.uber.org/atomic"
	"log"
	"time"

	"github.com/livekit/protocol/livekit"
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
		err = errors.New("Data undefined")
		return
	}
	dataBase64, ok := dataBase64Value.(string)
	if !ok {
		err = errors.New("cannot cast Data to string")
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
}

func NewRouterCommunicatorImpl(key livekit.RoomKey, mainDatabase *pubsub.DB, messageHandler func(ctx context.Context, roomKey livekit.RoomKey, msg *livekit.RTCNodeMessage) error) *RouterCommunicatorImpl {

	topic := "livekit_router_" + string(key)

	routerCommunicator := &RouterCommunicatorImpl{
		topic:          topic,
		key:            key,
		mainDatabase:   mainDatabase,
		messageHandler: messageHandler,
	}

	routerCommunicator.init()

	return routerCommunicator
}

func (c *RouterCommunicatorImpl) Close() {
	c.closed.Store(true)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*15)
	defer cancel()

	err := c.mainDatabase.Unsubscribe(ctx, c.topic)
	if err != nil {
		log.Printf("unsubscribed from topic err %v %v", c.topic, err)
	} else {
		log.Printf("unsubscribed from topic %v", c.topic)
	}
}

func (c *RouterCommunicatorImpl) Publish(message *livekit.RTCNodeMessage) {
	if c.closed.Load() {
		log.Printf("RouterCommunicatorImpl closed %v", c.key)
		return
	}

	data, err := proto.Marshal((*livekit.RTCNodeMessage)(message))
	if err != nil {
		log.Printf("RouterCommunicatorImpl Publish cannot marshal %v", message)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*15)
	defer cancel()

	if _, err := c.mainDatabase.Publish(ctx, c.topic, packRouterMessage(data)); err != nil {
		log.Printf("RouterCommunicatorImpl cannot publish %v", err)
	}
}

func (c *RouterCommunicatorImpl) init() {

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*15)
	defer cancel()

	subErr := c.mainDatabase.Subscribe(ctx, c.topic, c.dbHandler)
	if subErr != nil {
		log.Printf("RouterCommunicatorImpl cannot subscribe to topic %v", subErr)
	}
}

func (c *RouterCommunicatorImpl) dbHandler(event p2p_common.Event) {

	if event.FromPeerId == c.mainDatabase.GetHost().ID().String() {
		return
	}

	data, err := unpackRouterMessage(event.Message)
	if err != nil {
		log.Printf("RouterCommunicatorImpl dbHandler type err: %v %v", err, event)
		return
	}

	rm := livekit.RTCNodeMessage{}
	if err := proto.Unmarshal(data, &rm); err != nil {
		log.Printf("RouterCommunicatorImpl dbHandler cannot unmarshal: %v", event)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*15)
	defer cancel()

	err = c.messageHandler(ctx, c.key, &rm)
	if err != nil {
		log.Printf("RouterCommunicatorImpl dbHandler err: %v", err)
	}
}

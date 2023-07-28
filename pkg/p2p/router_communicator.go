package p2p

import (
	"context"
	"encoding/base64"
	"errors"
	"log"

	p2p_database "github.com/dTelecom/p2p-realtime-database"
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
	mainDatabase   *p2p_database.DB
	ctx            context.Context
	cancel         context.CancelFunc
	messageHandler func(ctx context.Context, roomKey livekit.RoomKey, msg *livekit.RTCNodeMessage) error
}

func NewRouterCommunicatorImpl(key livekit.RoomKey, mainDatabase *p2p_database.DB, messageHandler func(ctx context.Context, roomKey livekit.RoomKey, msg *livekit.RTCNodeMessage) error) *RouterCommunicatorImpl {

	ctx, cancel := context.WithCancel(context.Background())

	topic := "livekit_router_" + string(key)

	routerCommunicator := &RouterCommunicatorImpl{
		topic:          topic,
		key:            key,
		mainDatabase:   mainDatabase,
		ctx:            ctx,
		cancel:         cancel,
		messageHandler: messageHandler,
	}

	routerCommunicator.init()

	return routerCommunicator
}

func (c *RouterCommunicatorImpl) Close() {
	c.cancel()
}

func (c *RouterCommunicatorImpl) Publish(message *livekit.RTCNodeMessage) {

	data, err := proto.Marshal((*livekit.RTCNodeMessage)(message))
	if err != nil {
		log.Printf("RouterCommunicatorImpl Publish cannot marshal %v", message)
	}

	if _, err := c.mainDatabase.Publish(c.ctx, c.topic, packRouterMessage(data)); err != nil {
		log.Printf("RouterCommunicatorImpl cannot publish %v", err)
	}
}

func (c *RouterCommunicatorImpl) init() {

	subErr := c.mainDatabase.Subscribe(c.ctx, c.topic, c.dbHandler)
	if subErr != nil {
		log.Printf("RouterCommunicatorImpl cannot subscribe to topic %v", subErr)
	}
}

func (c *RouterCommunicatorImpl) dbHandler(event p2p_database.Event) {

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

	err = c.messageHandler(c.ctx, c.key, &rm)
	if err != nil {
		log.Printf("RouterCommunicatorImpl dbHandler err: %v", err)
	}
}

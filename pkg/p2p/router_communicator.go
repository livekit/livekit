package p2p

import (
	"context"
	"log"

	pubsub "github.com/dTelecom/pubsub-solana"
	"github.com/livekit/protocol/livekit"
	"google.golang.org/protobuf/proto"
)

type RouterCommunicatorImpl struct {
	topic          string
	key            livekit.RoomKey
	pubSub         *pubsub.PubSub
	ctx            context.Context
	cancel         context.CancelFunc
	messageHandler func(ctx context.Context, roomKey livekit.RoomKey, msg *livekit.RTCNodeMessage) error
}

func NewRouterCommunicatorImpl(key livekit.RoomKey, pubSub *pubsub.PubSub, messageHandler func(ctx context.Context, roomKey livekit.RoomKey, msg *livekit.RTCNodeMessage) error) *RouterCommunicatorImpl {

	ctx, cancel := context.WithCancel(context.Background())

	topic := "livekit_router_" + string(key)

	routerCommunicator := &RouterCommunicatorImpl{
		topic:          topic,
		key:            key,
		pubSub:         pubSub,
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

	data, err := proto.Marshal(message)
	if err != nil {
		log.Printf("RouterCommunicatorImpl Publish cannot marshal %v", message)
	}

	if _, err := c.pubSub.Publish(c.ctx, c.topic, data); err != nil {
		log.Printf("RouterCommunicatorImpl cannot publish %v", err)
	}
}

func (c *RouterCommunicatorImpl) init() {
	c.pubSub.Subscribe(c.topic, c.dbHandler)
}

func (c *RouterCommunicatorImpl) dbHandler(_ context.Context, event pubsub.Event) {

	if event.FromPeerId == c.pubSub.GetPeerId() {
		return
	}

	rm := livekit.RTCNodeMessage{}
	if err := proto.Unmarshal(event.Message, &rm); err != nil {
		log.Printf("RouterCommunicatorImpl dbHandler cannot unmarshal: %v", event)
		return
	}

	err := c.messageHandler(c.ctx, c.key, &rm)
	if err != nil {
		log.Printf("RouterCommunicatorImpl dbHandler err: %v", err)
	}
}

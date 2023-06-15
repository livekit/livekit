package p2p

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	p2p_database "github.com/dTelecom/p2p-realtime-database"
	logging "github.com/ipfs/go-log/v2"
	"github.com/livekit/protocol/livekit"
)

const (
	prefixIncomingMessageTopic = "incoming_messages_"
	prefixPeerKey              = "node_"
	pingMessage                = "ping"
	pongMessage                = "pong"
)

type RoomCommunicatorImpl struct {
	room *livekit.Room

	ctx    context.Context
	cancel context.CancelFunc

	db *p2p_database.DB

	peers           map[string]struct{}
	peerHandlers    []func(peerId string)
	messageHandlers []func(message interface{}, fromPeerId string, eventId string)

	mu sync.Mutex
}

func NewRoomCommunicatorImpl(room *livekit.Room, cfg p2p_database.Config) *RoomCommunicatorImpl {
	ctx, cancel := context.WithCancel(context.Background())
	roomCommunicator := &RoomCommunicatorImpl{
		room:   room,
		ctx:    ctx,
		cancel: cancel,
		peers:  make(map[string]struct{}),
	}

	cfg.DatabaseName = "livekit_room_" + room.Name
	_ = logging.SetLogLevel("*", "error")
	go roomCommunicator.init(cfg)

	return roomCommunicator
}

func (c *RoomCommunicatorImpl) Close() {
	removeErr := c.db.Remove(c.ctx, prefixPeerKey+c.db.GetHost().ID().String())
	if removeErr != nil {
		log.Fatalf("cannot remove")
	}
	c.cancel()
}

func (c *RoomCommunicatorImpl) init(cfg p2p_database.Config) {

	var (
		db *p2p_database.DB
	)

	cfg.NewKeyCallback = func(k string) {
		k = strings.TrimPrefix(k, "/")
		if !strings.HasPrefix(k, prefixPeerKey) {
			return
		}
		peerId := strings.TrimPrefix(k, prefixPeerKey)
		if peerId == db.GetHost().ID().String() {
			return
		}
		c.checkNewPeer(peerId)
	}

	db, err := p2p_database.Connect(c.ctx, cfg, logging.Logger("db_livekit_room_"+c.room.Name))
	if err != nil {
		log.Fatalf("cannot connect to database")
	}
	c.db = db

	subErr := db.Subscribe(c.ctx, p2pTopicName(db.GetHost().ID().String()), c.dbHandler)
	if subErr != nil {
		log.Fatalf("cannot subscribe to topic")
	}

	setErr := db.Set(c.ctx, prefixPeerKey+db.GetHost().ID().String(), time.Now().String())
	if setErr != nil {
		log.Fatalf("cannot set")
	}
}

func (c *RoomCommunicatorImpl) checkNewPeer(peerId string) {
	c.mu.Lock()
	if _, ok := c.peers[peerId]; ok {
		c.mu.Unlock()
	} else {
		c.peers[peerId] = struct{}{}
		for _, peerHandler := range c.peerHandlers {
			go peerHandler(peerId)
		}
		c.mu.Unlock()

		_, pubErr := c.db.Publish(c.ctx, p2pTopicName(peerId), pingMessage)
		if pubErr != nil {
			log.Fatalf("cannot send ping message for node %s in db %s: %s", peerId, c.room.Name, pubErr)
		} else {
			log.Println("PING message sent")
		}
	}
}

func (c *RoomCommunicatorImpl) dbHandler(event p2p_database.Event) {
	c.checkNewPeer(event.FromPeerId)
	if event.Message == pingMessage {
		log.Println("PING message received")
		if _, err := c.db.Publish(c.ctx, p2pTopicName(event.FromPeerId), pongMessage); err != nil {
			log.Fatalf("cannot send pong message for node %s in db %s: %s", event.FromPeerId, c.room.Name, err)
		} else {
			log.Println("PONG message sent")
		}
	} else if event.Message == pongMessage {
		log.Println("PONG message received")
	} else {
		c.mu.Lock()
		for _, messageHandler := range c.messageHandlers {
			go messageHandler(event.Message, event.FromPeerId, event.ID)
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
	event, err := c.db.Publish(c.ctx, p2pTopicName(peerId), message)
	return event.ID, err
}

func (c *RoomCommunicatorImpl) OnMessage(messageHandler func(message interface{}, fromPeerId string, eventId string)) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.messageHandlers = append(c.messageHandlers, messageHandler)
}

func p2pTopicName(peerId string) string {
	return prefixIncomingMessageTopic + peerId
}

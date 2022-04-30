package routing

import (
	"sync"

	"google.golang.org/protobuf/proto"
)

const DefaultMessageChannelSize = 200

type MessageChannel struct {
	msgChan  chan proto.Message
	onClose  func()
	isClosed bool
	lock     sync.RWMutex
}

func NewMessageChannel(size int) *MessageChannel {
	return &MessageChannel{
		// allow some buffer to avoid blocked writes
		msgChan: make(chan proto.Message, size),
	}
}

func (m *MessageChannel) OnClose(f func()) {
	m.onClose = f
}

func (m *MessageChannel) IsClosed() bool {
	m.lock.RLock()
	defer m.lock.RUnlock()
	return m.isClosed
}

func (m *MessageChannel) WriteMessage(msg proto.Message) error {
	m.lock.RLock()
	defer m.lock.RUnlock()
	if m.isClosed {
		return ErrChannelClosed
	}

	select {
	case m.msgChan <- msg:
		// published
		return nil
	default:
		// channel is full
		return ErrChannelFull
	}
}

func (m *MessageChannel) ReadChan() <-chan proto.Message {
	return m.msgChan
}

func (m *MessageChannel) Close() {
	m.lock.Lock()
	if m.isClosed {
		m.lock.Unlock()
		return
	}
	m.isClosed = true
	close(m.msgChan)
	m.lock.Unlock()

	if m.onClose != nil {
		m.onClose()
	}
}

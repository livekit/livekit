package routing

import (
	"google.golang.org/protobuf/proto"
)

const DefaultMessageChannelSize = 200

type MessageChannel struct {
	msgChan chan proto.Message
	closed  chan struct{}
	onClose func()
}

func NewMessageChannel(size int) *MessageChannel {
	return &MessageChannel{
		// allow some buffer to avoid blocked writes
		msgChan: make(chan proto.Message, size),
		closed:  make(chan struct{}),
	}
}

func (m *MessageChannel) OnClose(f func()) {
	m.onClose = f
}

func (m *MessageChannel) IsClosed() bool {
	select {
	case <-m.closed:
		return true
	default:
		return false
	}
}

func (m *MessageChannel) WriteMessage(msg proto.Message) error {
	if m.IsClosed() {
		return ErrChannelClosed
	}

	select {
	case <-m.closed:
		return ErrChannelClosed
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
	if m.IsClosed() {
		return
	}
	close(m.closed)
	close(m.msgChan)
	if m.onClose != nil {
		m.onClose()
	}
}

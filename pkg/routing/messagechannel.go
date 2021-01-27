package routing

import (
	"google.golang.org/protobuf/proto"
)

type MessageChannel struct {
	msgChan  chan proto.Message
	isClosed bool
	onClose  func()
}

func NewMessageChannel() *MessageChannel {
	return &MessageChannel{
		// allow some buffer to avoid blocked writes
		msgChan: make(chan proto.Message, 2),
	}
}

func (m *MessageChannel) OnClose(f func()) {
	m.onClose = f
}

func (m *MessageChannel) WriteMessage(msg proto.Message) error {
	if m.isClosed {
		return ErrChannelClosed
	}
	m.msgChan <- msg
	return nil
}

func (m *MessageChannel) ReadChan() <-chan proto.Message {
	return m.msgChan
}

func (m *MessageChannel) Close() {
	m.isClosed = true
	close(m.msgChan)
	if m.onClose != nil {
		m.onClose()
	}
}

package routing

import (
	"google.golang.org/protobuf/proto"
)

type MessageChannel struct {
	msgChan chan proto.Message
	onClose func()
}

func NewMessageChannel() *MessageChannel {
	return &MessageChannel{
		msgChan: make(chan proto.Message, 1),
	}
}

func (m *MessageChannel) OnClose(f func()) {
	m.onClose = f
}

func (m *MessageChannel) WriteMessage(msg proto.Message) error {
	m.msgChan <- msg
	return nil
}

func (m *MessageChannel) ReadChan() <-chan proto.Message {
	return m.msgChan
}

func (m *MessageChannel) Close() {
	close(m.msgChan)
	if m.onClose != nil {
		m.onClose()
	}
}

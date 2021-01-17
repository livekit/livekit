package routing

import (
	"io"

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

func (m *MessageChannel) ReadMessage() (proto.Message, error) {
	msg := <-m.msgChan
	// channel closed
	if msg == nil {
		return nil, io.EOF
	}
	return msg, nil
}

func (m *MessageChannel) Close() {
	close(m.msgChan)
	if m.onClose != nil {
		m.onClose()
	}
}

package routing

import (
	"io"
)

type MessageChannel struct {
	msgChan chan interface{}
}

func NewMessageChannel() *MessageChannel {
	return &MessageChannel{
		msgChan: make(chan interface{}, 1),
	}
}

func (m *MessageChannel) WriteMessage(msg interface{}) error {
	m.msgChan <- msg
	return nil
}

func (m *MessageChannel) ReadMessage() (interface{}, error) {
	msg := <-m.msgChan
	// channel closed
	if msg == nil {
		return nil, io.EOF
	}
	return msg, nil
}

func (m *MessageChannel) Close() {
	close(m.msgChan)
}

package routing

import (
	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/pkg/utils"
)

type MessageChannel struct {
	msgChan  chan proto.Message
	isClosed utils.AtomicFlag
	onClose  func()
}

func NewMessageChannel() *MessageChannel {
	return &MessageChannel{
		// allow some buffer to avoid blocked writes
		msgChan: make(chan proto.Message, 10),
	}
}

func (m *MessageChannel) OnClose(f func()) {
	m.onClose = f
}

func (m *MessageChannel) WriteMessage(msg proto.Message) error {
	if m.isClosed.Get() {
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
	if !m.isClosed.TrySet(true) {
		return
	}
	close(m.msgChan)
	if m.onClose != nil {
		m.onClose()
	}
}

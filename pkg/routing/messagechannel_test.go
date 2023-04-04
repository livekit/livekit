package routing_test

import (
	"sync"
	"testing"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/routing"
)

func TestMessageChannel_WriteMessageClosed(t *testing.T) {
	// ensure it doesn't panic when written to after closing
	m := routing.NewMessageChannel(routing.DefaultMessageChannelSize)
	go func() {
		for msg := range m.ReadChan() {
			if msg == nil {
				return
			}
		}
	}()

	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			_ = m.WriteMessage(&livekit.RTCNodeMessage{})
		}
	}()
	_ = m.WriteMessage(&livekit.RTCNodeMessage{})
	m.Close()
	_ = m.WriteMessage(&livekit.RTCNodeMessage{})

	wg.Wait()
}

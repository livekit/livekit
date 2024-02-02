// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package routing_test

import (
	"sync"
	"testing"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/routing"
)

func TestMessageChannel_WriteMessageClosed(t *testing.T) {
	// ensure it doesn't panic when written to after closing
	m := routing.NewMessageChannel(livekit.ConnectionID("test"), routing.DefaultMessageChannelSize)
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
			_ = m.WriteMessage(&livekit.SignalRequest{})
		}
	}()
	_ = m.WriteMessage(&livekit.SignalRequest{})
	m.Close()
	_ = m.WriteMessage(&livekit.SignalRequest{})

	wg.Wait()
}

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

package pacer

import (
	"sync"

	"github.com/frostbyte73/core"
	"github.com/gammazero/deque"
	"github.com/livekit/livekit-server/pkg/sfu/bwe"
	"github.com/livekit/protocol/logger"
)

type NoQueue struct {
	*Base

	logger logger.Logger

	lock    sync.RWMutex
	packets deque.Deque[*Packet]
	wake    chan struct{}
	stop    core.Fuse
}

func NewNoQueue(logger logger.Logger, bwe bwe.BWE) *NoQueue {
	n := &NoQueue{
		Base:   NewBase(logger, bwe),
		logger: logger,
		wake:   make(chan struct{}, 1),
	}
	n.packets.SetBaseCap(512)

	go n.sendWorker()
	return n
}

func (n *NoQueue) Stop() {
	n.stop.Break()

	select {
	case n.wake <- struct{}{}:
	default:
	}
}

func (n *NoQueue) Enqueue(p *Packet) {
	n.lock.Lock()
	n.packets.PushBack(p)
	n.lock.Unlock()

	select {
	case n.wake <- struct{}{}:
	default:
	}
}

func (n *NoQueue) sendWorker() {
	for {
		<-n.wake
		for {
			if n.stop.IsBroken() {
				return
			}

			n.lock.Lock()
			if n.packets.Len() == 0 {
				n.lock.Unlock()
				break
			}
			p := n.packets.PopFront()
			n.lock.Unlock()

			n.Base.SendPacket(p)
		}
	}
}

// ------------------------------------------------

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

package utils

import (
	"math/bits"
	"sync"

	"github.com/gammazero/deque"
	"github.com/livekit/protocol/utils"
)

type OpsQueue struct {
	name        string
	flushOnStop bool

	lock      sync.Mutex
	ops       deque.Deque[func()]
	wake      chan struct{}
	isStarted bool
	doneChan  chan struct{}
	isStopped bool
}

func NewOpsQueue(name string, minSize uint, flushOnStop bool) *OpsQueue {
	oq := &OpsQueue{
		name:        name,
		flushOnStop: flushOnStop,
		wake:        make(chan struct{}, 1),
		doneChan:    make(chan struct{}),
	}
	oq.ops.SetMinCapacity(uint(utils.Min(bits.Len64(uint64(minSize-1)), 16)))
	return oq
}

func (oq *OpsQueue) Start() {
	oq.lock.Lock()
	if oq.isStarted {
		oq.lock.Unlock()
		return
	}

	oq.isStarted = true
	oq.lock.Unlock()

	go oq.process()
}

func (oq *OpsQueue) Stop() <-chan struct{} {
	oq.lock.Lock()
	if oq.isStopped {
		oq.lock.Unlock()
		return oq.doneChan
	}

	oq.isStopped = true
	close(oq.wake)
	oq.lock.Unlock()
	return oq.doneChan
}

func (oq *OpsQueue) Enqueue(op func()) {
	oq.lock.Lock()
	defer oq.lock.Unlock()

	oq.ops.PushBack(op)
	if oq.ops.Len() == 1 && !oq.isStopped {
		select {
		case oq.wake <- struct{}{}:
		default:
		}
	}
}

func (oq *OpsQueue) process() {
	defer close(oq.doneChan)

	for {
		<-oq.wake
		for {
			oq.lock.Lock()
			if oq.isStopped && (!oq.flushOnStop || oq.ops.Len() == 0) {
				oq.lock.Unlock()
				return
			}

			if oq.ops.Len() == 0 {
				oq.lock.Unlock()
				break
			}
			op := oq.ops.PopFront()
			oq.lock.Unlock()

			op()
		}
	}
}

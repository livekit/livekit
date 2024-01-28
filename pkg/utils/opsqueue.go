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
	"sync"

	"github.com/gammazero/deque"
)

type OpsQueue struct {
	name string

	lock      sync.Mutex
	finalize  func()
	ops       deque.Deque[func()]
	wake      chan struct{}
	isStarted bool
	isStopped bool
}

func NewOpsQueue(name string, minSize uint) *OpsQueue {
	oq := &OpsQueue{
		name: name,
		wake: make(chan struct{}, 1),
	}
	if minSize != 0 {
		minSizeExp := uint(0)
		for {
			if (1<<minSizeExp) >= minSize || minSizeExp == 16 {
				// guard against too large a min size
				break
			}
			minSizeExp++
		}
		oq.ops.SetMinCapacity(minSizeExp)
	}
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

func (oq *OpsQueue) Stop() {
	oq.lock.Lock()
	if oq.isStopped {
		oq.lock.Unlock()
		return
	}

	oq.isStopped = true
	close(oq.wake)
	oq.lock.Unlock()
}

func (oq *OpsQueue) SetFinalize(f func()) {
	oq.lock.Lock()
	oq.finalize = f
	oq.lock.Unlock()
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
done:
	for {
		<-oq.wake
		for {
			oq.lock.Lock()
			if oq.isStopped {
				oq.lock.Unlock()
				break done
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

	oq.lock.Lock()
	finalize := oq.finalize
	oq.lock.Unlock()
	if finalize != nil {
		finalize()
	}
}

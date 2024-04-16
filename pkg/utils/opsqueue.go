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

	"github.com/livekit/protocol/logger"
)

type OpsQueueParams struct {
	Name        string
	MinSize     uint
	FlushOnStop bool
	Logger      logger.Logger
}

type untypedOpsQueueOp func()

func (it untypedOpsQueueOp) run() {
	it()
}

type OpsQueue struct {
	OpsQueueBase[untypedOpsQueueOp]
}

func NewOpsQueue(params OpsQueueParams) *OpsQueue {
	return &OpsQueue{
		OpsQueueBase: *newOpsQueueBase[untypedOpsQueueOp](params),
	}
}

type typedOpsQueueOp[T any] struct {
	fn  func(T)
	arg T
}

func (it typedOpsQueueOp[T]) run() {
	it.fn(it.arg)
}

type TypedOpsQueue[T any] struct {
	OpsQueueBase[typedOpsQueueOp[T]]
}

func NewTypedOpsQueue[T any](params OpsQueueParams) *TypedOpsQueue[T] {
	return &TypedOpsQueue[T]{
		OpsQueueBase: *newOpsQueueBase[typedOpsQueueOp[T]](params),
	}
}

func (oq *TypedOpsQueue[T]) Enqueue(fn func(T), arg T) {
	oq.OpsQueueBase.Enqueue(typedOpsQueueOp[T]{fn, arg})
}

type opsQueueItem interface {
	run()
}

type OpsQueueBase[T opsQueueItem] struct {
	params OpsQueueParams

	lock      sync.Mutex
	ops       deque.Deque[T]
	wake      chan struct{}
	isStarted bool
	doneChan  chan struct{}
	isStopped bool
}

func newOpsQueueBase[T opsQueueItem](params OpsQueueParams) *OpsQueueBase[T] {
	oq := &OpsQueueBase[T]{
		params:   params,
		wake:     make(chan struct{}, 1),
		doneChan: make(chan struct{}),
	}
	oq.ops.SetMinCapacity(uint(min(bits.Len64(uint64(oq.params.MinSize-1)), 7)))
	return oq
}

func (oq *OpsQueueBase[T]) Start() {
	oq.lock.Lock()
	if oq.isStarted {
		oq.lock.Unlock()
		return
	}

	oq.isStarted = true
	oq.lock.Unlock()

	go oq.process()
}

func (oq *OpsQueueBase[T]) Stop() <-chan struct{} {
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

func (oq *OpsQueueBase[T]) Enqueue(op T) {
	oq.lock.Lock()
	defer oq.lock.Unlock()

	if oq.isStopped {
		return
	}

	oq.ops.PushBack(op)
	if oq.ops.Len() == 1 {
		select {
		case oq.wake <- struct{}{}:
		default:
		}
	}
}

func (oq *OpsQueueBase[T]) process() {
	defer close(oq.doneChan)

	for {
		<-oq.wake
		for {
			oq.lock.Lock()
			if oq.isStopped && (!oq.params.FlushOnStop || oq.ops.Len() == 0) {
				oq.lock.Unlock()
				return
			}

			if oq.ops.Len() == 0 {
				oq.lock.Unlock()
				break
			}
			op := oq.ops.PopFront()
			oq.lock.Unlock()

			op.run()
		}
	}
}

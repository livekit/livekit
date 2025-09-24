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

	"github.com/livekit/protocol/logger"
)

type OpsQueueParams struct {
	Name        string
	MinSize     uint
	FlushOnStop bool
	Logger      logger.Logger
}

type UntypedQueueOp func()

func (op UntypedQueueOp) run() {
	op()
}

type OpsQueue struct {
	opsQueueBase[UntypedQueueOp]
}

func NewOpsQueue(params OpsQueueParams) *OpsQueue {
	return &OpsQueue{*newOpsQueueBase[UntypedQueueOp](params)}
}

type typedQueueOp[T any] struct {
	fn  func(T)
	arg T
}

func (op typedQueueOp[T]) run() {
	op.fn(op.arg)
}

type TypedOpsQueue[T any] struct {
	opsQueueBase[typedQueueOp[T]]
}

func NewTypedOpsQueue[T any](params OpsQueueParams) *TypedOpsQueue[T] {
	return &TypedOpsQueue[T]{*newOpsQueueBase[typedQueueOp[T]](params)}
}

func (oq *TypedOpsQueue[T]) Enqueue(fn func(T), arg T) {
	oq.opsQueueBase.Enqueue(typedQueueOp[T]{fn, arg})
}

type opsQueueItem interface {
	run()
}

type opsQueueBase[T opsQueueItem] struct {
	params OpsQueueParams

	lock      sync.Mutex
	ops       deque.Deque[T]
	wake      chan struct{}
	isStarted bool
	doneChan  chan struct{}
	isStopped bool
}

func newOpsQueueBase[T opsQueueItem](params OpsQueueParams) *opsQueueBase[T] {
	o := &opsQueueBase[T]{
		params:   params,
		wake:     make(chan struct{}, 1),
		doneChan: make(chan struct{}),
	}
	o.ops.SetBaseCap(int(min(params.MinSize, 128)))
	return o
}

func (oq *opsQueueBase[T]) Start() {
	oq.lock.Lock()
	if oq.isStarted {
		oq.lock.Unlock()
		return
	}

	oq.isStarted = true
	oq.lock.Unlock()

	go oq.process()
}

func (oq *opsQueueBase[T]) Stop() <-chan struct{} {
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

func (oq *opsQueueBase[T]) Enqueue(op T) {
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
			oq.params.Logger.Infow("could not wake ops queue", "name", oq.params.Name)
		}
	}
}

func (oq *opsQueueBase[T]) process() {
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

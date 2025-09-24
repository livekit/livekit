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

	"github.com/livekit/protocol/logger"
)

type OpsQueueParams struct {
	Name        string
	MinSize     uint
	FlushOnStop bool
	Logger      logger.Logger
}

// ----------------------------

type UntypedQueueOp func()

func (op UntypedQueueOp) run() {
	op()
}

// ----------------------------

type OpsQueue struct {
	opsQueueBase[UntypedQueueOp]
}

func NewOpsQueue(params OpsQueueParams) *OpsQueue {
	return &OpsQueue{*newOpsQueueBase[UntypedQueueOp](params)}
}

// ----------------------------

type typedQueueOp[T any] struct {
	fn  func(T)
	arg T
}

func (op typedQueueOp[T]) run() {
	op.fn(op.arg)
}

// ----------------------------

type TypedOpsQueue[T any] struct {
	opsQueueBase[typedQueueOp[T]]
}

func NewTypedOpsQueue[T any](params OpsQueueParams) *TypedOpsQueue[T] {
	return &TypedOpsQueue[T]{*newOpsQueueBase[typedQueueOp[T]](params)}
}

func (oq *TypedOpsQueue[T]) Enqueue(fn func(T), arg T) {
	oq.opsQueueBase.Enqueue(typedQueueOp[T]{fn, arg})
}

// ----------------------------

type opsQueueItem interface {
	run()
}

type element struct {
	opsQueueItem
	next *element
}

// ----------------------------

type opsQueueBase[T opsQueueItem] struct {
	params OpsQueueParams

	lock      sync.Mutex
	opsHead   *element
	opsTail   *element
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

	if oq.opsHead == nil {
		oq.opsHead = &element{op, nil}
		oq.opsTail = oq.opsHead

		// wake up on first entry in queue
		select {
		case oq.wake <- struct{}{}:
		default:
		}
	} else {
		elem := &element{op, nil}
		oq.opsTail.next = elem
		oq.opsTail = elem
	}
}

func (oq *opsQueueBase[T]) process() {
	defer close(oq.doneChan)

	for {
		<-oq.wake
		for {
			oq.lock.Lock()
			if oq.isStopped && (!oq.params.FlushOnStop || oq.opsHead == nil) {
				oq.lock.Unlock()
				return
			}

			elem := oq.opsHead
			if elem == nil {
				oq.lock.Unlock()
				break
			}

			oq.opsHead = elem.next
			if oq.opsHead == nil {
				oq.opsTail = nil
			}
			oq.lock.Unlock()

			elem.opsQueueItem.run()
		}
	}
}

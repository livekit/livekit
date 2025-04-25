/*
 * Copyright 2024 LiveKit, Inc
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package utils

import (
	"sync"

	"github.com/frostbyte73/core"
)

// IncrementalDispatcher is a dispatcher that allows multiple consumers to consume items as they become
// available, while producers can add items at anytime.
type IncrementalDispatcher[T any] struct {
	done  core.Fuse
	lock  sync.RWMutex
	cond  *sync.Cond
	items []T
}

func NewIncrementalDispatcher[T any]() *IncrementalDispatcher[T] {
	p := &IncrementalDispatcher[T]{}
	p.cond = sync.NewCond(&p.lock)
	return p
}

func (d *IncrementalDispatcher[T]) Add(item T) {
	if d.done.IsBroken() {
		return
	}
	d.lock.Lock()
	d.items = append(d.items, item)
	d.cond.Broadcast()
	d.lock.Unlock()
}

func (d *IncrementalDispatcher[T]) Done() {
	d.lock.Lock()
	d.done.Break()
	d.cond.Broadcast()
	d.lock.Unlock()
}

func (d *IncrementalDispatcher[T]) ForEach(fn func(T)) {
	idx := 0
	dispatchFromIdx := func() {
		var itemsToDispatch []T
		d.lock.RLock()
		for idx < len(d.items) {
			itemsToDispatch = append(itemsToDispatch, d.items[idx])
			idx++
		}
		d.lock.RUnlock()
		for _, item := range itemsToDispatch {
			fn(item)
		}
	}
	for !d.done.IsBroken() {
		dispatchFromIdx()
		d.lock.Lock()
		// need to check again because Done may have been called while dispatching
		if d.done.IsBroken() {
			d.lock.Unlock()
			break
		}
		if idx == len(d.items) {
			d.cond.Wait()
		}
		d.lock.Unlock()
	}

	dispatchFromIdx()
}

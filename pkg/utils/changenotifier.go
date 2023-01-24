/*
 * Copyright 2023 LiveKit, Inc
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
)

type ChangeNotifier struct {
	observers sync.Map
}

func NewChangeNotifier() *ChangeNotifier {
	return &ChangeNotifier{}
}

func (n *ChangeNotifier) AddObserver(key string, onChanged func()) {
	n.observers.Store(key, onChanged)
}

func (n *ChangeNotifier) RemoveObserver(key string) {
	n.observers.Delete(key)
}

func (n *ChangeNotifier) HasObservers() bool {
	hasObservers := false
	n.observers.Range(func(key, val any) bool {
		hasObservers = true
		return false
	})
	return hasObservers
}

func (n *ChangeNotifier) NotifyChanged() {
	go func() {
		n.observers.Range(func(key, val any) bool {
			if fn, ok := val.(func()); ok {
				fn()
			}
			return true
		})
	}()
}

type ChangeNotifierManager struct {
	lock      sync.Mutex
	notifiers map[string]*ChangeNotifier
}

func NewChangeNotifierManager() *ChangeNotifierManager {
	return &ChangeNotifierManager{
		notifiers: make(map[string]*ChangeNotifier),
	}
}

func (m *ChangeNotifierManager) GetNotifier(key string) *ChangeNotifier {
	m.lock.Lock()
	defer m.lock.Unlock()

	return m.notifiers[key]
}

func (m *ChangeNotifierManager) GetOrCreateNotifier(key string) *ChangeNotifier {
	m.lock.Lock()
	defer m.lock.Unlock()

	if notifier, ok := m.notifiers[key]; ok {
		return notifier
	}

	notifier := NewChangeNotifier()
	m.notifiers[key] = notifier
	return notifier
}

func (m *ChangeNotifierManager) RemoveNotifier(key string, force bool) {
	m.lock.Lock()
	defer m.lock.Unlock()

	if notifier, ok := m.notifiers[key]; ok {
		if force || !notifier.HasObservers() {
			delete(m.notifiers, key)
		}
	}
}

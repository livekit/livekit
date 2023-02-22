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

import "sync"

type ChangeNotifier struct {
	lock      sync.Mutex
	observers map[string]func()
}

func NewChangeNotifier() *ChangeNotifier {
	return &ChangeNotifier{
		observers: make(map[string]func()),
	}
}

func (n *ChangeNotifier) AddObserver(key string, onChanged func()) {
	n.lock.Lock()
	defer n.lock.Unlock()

	n.observers[key] = onChanged
}

func (n *ChangeNotifier) RemoveObserver(key string) {
	n.lock.Lock()
	defer n.lock.Unlock()

	delete(n.observers, key)
}

func (n *ChangeNotifier) HasObservers() bool {
	n.lock.Lock()
	defer n.lock.Unlock()

	return len(n.observers) > 0
}

func (n *ChangeNotifier) NotifyChanged() {
	n.lock.Lock()
	if len(n.observers) == 0 {
		n.lock.Unlock()
		return
	}
	observers := make([]func(), 0, len(n.observers))
	for _, f := range n.observers {
		observers = append(observers, f)
	}
	n.lock.Unlock()

	go func() {
		for _, f := range observers {
			f()
		}
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

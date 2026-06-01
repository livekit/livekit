// Copyright 2026 LiveKit, Inc.
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
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestChangeNotifier(t *testing.T) {
	t.Run("Observer management", func(t *testing.T) {
		notifier := NewChangeNotifier()
		require.False(t, notifier.HasObservers())

		called := false
		notifier.AddObserver("test-key", func() {
			called = true
		})
		require.True(t, notifier.HasObservers())

		notifier.RemoveObserver("test-key")
		require.False(t, notifier.HasObservers())
		require.False(t, called)
	})

	t.Run("Notification triggers callbacks asynchronously", func(t *testing.T) {
		notifier := NewChangeNotifier()
		var wg sync.WaitGroup
		wg.Add(2)

		var mu sync.Mutex
		callCounts := make(map[string]int)

		notifier.AddObserver("obs1", func() {
			mu.Lock()
			callCounts["obs1"]++
			mu.Unlock()
			wg.Done()
		})

		notifier.AddObserver("obs2", func() {
			mu.Lock()
			callCounts["obs2"]++
			mu.Unlock()
			wg.Done()
		})

		notifier.NotifyChanged()

		// Wait for async execution of observers
		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()

		select {
		case <-done:
			// Success
		case <-time.After(1 * time.Second):
			t.Fatal("Timeout waiting for change notification callbacks")
		}

		mu.Lock()
		require.Equal(t, 1, callCounts["obs1"])
		require.Equal(t, 1, callCounts["obs2"])
		mu.Unlock()
	})
}

func TestChangeNotifierManager(t *testing.T) {
	t.Run("Get and Create Notifiers", func(t *testing.T) {
		manager := NewChangeNotifierManager()
		require.Nil(t, manager.GetNotifier("non-existent"))

		notifier := manager.GetOrCreateNotifier("room1")
		require.NotNil(t, notifier)

		retrieved := manager.GetNotifier("room1")
		require.Equal(t, notifier, retrieved)

		// GetOrCreate should return the existing one
		again := manager.GetOrCreateNotifier("room1")
		require.Equal(t, notifier, again)
	})

	t.Run("Remove Notifiers with HasObservers check", func(t *testing.T) {
		manager := NewChangeNotifierManager()
		_ = manager.GetOrCreateNotifier("room1")

		// Case 1: notifier has no observers, should be removed
		manager.RemoveNotifier("room1", false)
		require.Nil(t, manager.GetNotifier("room1"))

		// Re-create and add an observer
		notifier := manager.GetOrCreateNotifier("room1")
		notifier.AddObserver("observer", func() {})

		// Case 2: notifier has observer, RemoveNotifier(..., false) should not remove it
		manager.RemoveNotifier("room1", false)
		require.NotNil(t, manager.GetNotifier("room1"))

		// Case 3: notifier has observer, RemoveNotifier(..., true) (force) should remove it
		manager.RemoveNotifier("room1", true)
		require.Nil(t, manager.GetNotifier("room1"))
	})
}

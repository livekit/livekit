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

package utils_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"go.uber.org/atomic"

	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/testutils"
	"github.com/livekit/livekit-server/pkg/utils"
)

func TestForEach(t *testing.T) {
	producer := utils.NewIncrementalDispatcher[int]()
	go func() {
		defer producer.Done()
		producer.Add(1)
		producer.Add(2)
		producer.Add(3)
	}()

	sum := 0
	producer.ForEach(func(item int) {
		sum += item
	})

	require.Equal(t, 6, sum)
}

func TestConcurrentConsumption(t *testing.T) {
	producer := utils.NewIncrementalDispatcher[int]()
	numConsumers := 100
	sums := make([]atomic.Int32, numConsumers)
	var wg sync.WaitGroup

	for i := 0; i < numConsumers; i++ {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			producer.ForEach(func(item int) {
				sums[i].Add(int32(item))
			})
		}()
	}

	// Add items
	expectedSum := 0
	for i := 0; i < 20; i++ {
		expectedSum += i
		producer.Add(i)
	}

	for i := 0; i < numConsumers; i++ {
		testutils.WithTimeout(t, func() string {
			if sums[i].Load() != int32(expectedSum) {
				return fmt.Sprintf("consumer %d did not consume all the items. expected %d, actual: %d",
					i, expectedSum, sums[i].Load())
			}
			return ""
		}, time.Second)
	}

	// keep adding and ensure it's consumed
	for i := 20; i < 30; i++ {
		expectedSum += i
		producer.Add(i)
	}

	// wait for all consumers to finish
	producer.Done()
	wg.Wait()

	for i := 0; i < numConsumers; i++ {
		require.Equal(t, int32(expectedSum), sums[i].Load(), "consumer %d did not match", i)
	}
}

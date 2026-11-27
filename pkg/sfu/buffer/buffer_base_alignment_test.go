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

package buffer

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBufferBasePLIAtomicAlignment(t *testing.T) {
	// Embedding after a byte also exercises alignment when BufferBase is not
	// the first field of an allocation on a 32-bit target.
	var buffers [2]struct {
		prefix byte
		buffer BufferBase
	}
	for i := range buffers {
		b := &buffers[i].buffer
		b.lastPliAt.Store(100)
		require.EqualValues(t, 100, b.lastPliAt.Load())
		require.True(t, b.lastPliAt.CompareAndSwap(100, 200))
		require.EqualValues(t, 200, b.lastPliAt.Load())
	}
}

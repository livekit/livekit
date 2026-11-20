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

package rtc

import (
	"encoding/binary"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
)

func TestUserPacketDeduper(t *testing.T) {
	packet := func(n uint64) *livekit.UserPacket {
		var nonce uuid.UUID
		binary.BigEndian.PutUint64(nonce[8:], n)
		return &livekit.UserPacket{Nonce: nonce[:]}
	}

	t.Run("zero nonce is a valid first packet", func(t *testing.T) {
		d := NewUserPacketDeduper()
		require.False(t, d.IsDuplicate(packet(0)))
		require.True(t, d.IsDuplicate(packet(0)))
	})

	t.Run("zero nonce can follow another packet", func(t *testing.T) {
		d := NewUserPacketDeduper()
		require.False(t, d.IsDuplicate(packet(1)))
		require.False(t, d.IsDuplicate(packet(0)))
		require.True(t, d.IsDuplicate(packet(1)))
		require.True(t, d.IsDuplicate(packet(0)))
	})

	t.Run("invalid nonces do not enter the history", func(t *testing.T) {
		d := NewUserPacketDeduper()
		for _, size := range []int{0, 1, 15, 17} {
			p := &livekit.UserPacket{Nonce: make([]byte, size)}
			require.False(t, d.IsDuplicate(p))
			require.False(t, d.IsDuplicate(p))
		}
		require.Empty(t, d.seen)
	})

	t.Run("retains exactly the latest unique packets", func(t *testing.T) {
		d := NewUserPacketDeduper()
		for n := uint64(0); n < 3*maxSize; n++ {
			require.False(t, d.IsDuplicate(packet(n)), "packet %d", n)
			first := uint64(0)
			if n >= maxSize {
				first = n - maxSize + 1
			}
			for retained := first; retained <= n; retained++ {
				require.True(t, d.IsDuplicate(packet(retained)), "retained packet %d", retained)
			}
			require.LessOrEqual(t, len(d.seen), maxSize)
		}
		require.False(t, d.IsDuplicate(packet(0)), "the oldest packet must have been evicted")
	})

	t.Run("concurrent copies are accepted exactly once", func(t *testing.T) {
		d := NewUserPacketDeduper()
		p := packet(0)
		var accepted atomic.Int32
		var wg sync.WaitGroup
		for range 100 {
			wg.Go(func() {
				if !d.IsDuplicate(p) {
					accepted.Add(1)
				}
			})
		}
		wg.Wait()
		require.EqualValues(t, 1, accepted.Load())
	})
}

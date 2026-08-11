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

package service

import (
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func udpAddr(t *testing.T, port int) net.Addr {
	t.Helper()
	return &net.UDPAddr{IP: net.IPv4(10, 0, 0, 1), Port: port}
}

func TestTURNAllocationQuota_LimitsPerUser(t *testing.T) {
	q := newTURNAllocationQuota(4)

	// four distinct source 5-tuples for the same participant are allowed
	for i := 0; i < 4; i++ {
		require.True(t, q.Allow("participantA", LivekitRealm, udpAddr(t, 5000+i)), "allocation %d", i)
	}
	// the fifth is rejected (Pion returns 486 Allocation Quota Reached)
	require.False(t, q.Allow("participantA", LivekitRealm, udpAddr(t, 6000)))
}

func TestTURNAllocationQuota_IsolatedPerUser(t *testing.T) {
	q := newTURNAllocationQuota(2)

	require.True(t, q.Allow("A", LivekitRealm, udpAddr(t, 5000)))
	require.True(t, q.Allow("A", LivekitRealm, udpAddr(t, 5001)))
	require.False(t, q.Allow("A", LivekitRealm, udpAddr(t, 5002)))

	// a different participant has its own budget
	require.True(t, q.Allow("B", LivekitRealm, udpAddr(t, 5000)))
	require.True(t, q.Allow("B", LivekitRealm, udpAddr(t, 5001)))
	require.False(t, q.Allow("B", LivekitRealm, udpAddr(t, 5002)))
}

func TestTURNAllocationQuota_ReleaseFreesSlot(t *testing.T) {
	q := newTURNAllocationQuota(1)

	addr := udpAddr(t, 5000)
	require.True(t, q.Allow("A", LivekitRealm, addr))
	require.False(t, q.Allow("A", LivekitRealm, udpAddr(t, 5001)))

	// once the allocation is deleted the slot is reclaimed
	q.OnDeleted(addr, nil, "udp", "A", LivekitRealm)
	require.True(t, q.Allow("A", LivekitRealm, udpAddr(t, 5001)))
}

func TestTURNAllocationQuota_RetransmitIsIdempotent(t *testing.T) {
	q := newTURNAllocationQuota(1)

	addr := udpAddr(t, 5000)
	// same 5-tuple retried multiple times must consume only one slot
	require.True(t, q.Allow("A", LivekitRealm, addr))
	require.True(t, q.Allow("A", LivekitRealm, addr))
	require.True(t, q.Allow("A", LivekitRealm, addr))

	// a different 5-tuple is still over quota
	require.False(t, q.Allow("A", LivekitRealm, udpAddr(t, 5001)))
}

func TestTURNAllocationQuota_ForgetsUserWhenEmpty(t *testing.T) {
	q := newTURNAllocationQuota(1)

	addr := udpAddr(t, 5000)
	require.True(t, q.Allow("A", LivekitRealm, addr))
	q.OnDeleted(addr, nil, "udp", "A", LivekitRealm)

	q.mu.Lock()
	_, tracked := q.users["A"]
	q.mu.Unlock()
	require.False(t, tracked, "user with no active allocations should not be retained")
}

// TestTURNAllocationQuota_ConcurrentAllocatesRespectLimit is the core security
// property: a burst of concurrent Allocate requests reusing one credential must
// not be able to race past the limit.
func TestTURNAllocationQuota_ConcurrentAllocatesRespectLimit(t *testing.T) {
	const limit = 4
	q := newTURNAllocationQuota(limit)

	var granted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(port int) {
			defer wg.Done()
			if q.Allow("attacker", LivekitRealm, udpAddr(t, port)) {
				granted.Add(1)
			}
		}(7000 + i)
	}
	wg.Wait()

	require.Equal(t, int32(limit), granted.Load(), "concurrent allocations must not exceed the quota")
}

func TestTURNAllocationQuota_SrcAddrKeyDistinguishesNetwork(t *testing.T) {
	udp := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 5000}
	tcp := &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 5000}
	require.NotEqual(t, srcAddrKey(udp), srcAddrKey(tcp))
	require.Equal(t, "", srcAddrKey(nil))
}

func BenchmarkTURNAllocationQuota_Allow(b *testing.B) {
	q := newTURNAllocationQuota(1000000)
	addrs := make([]net.Addr, 0, 64)
	for i := 0; i < 64; i++ {
		addrs = append(addrs, &net.UDPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 5000 + i})
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q.Allow(fmt.Sprintf("u%d", i%128), LivekitRealm, addrs[i%len(addrs)])
	}
}

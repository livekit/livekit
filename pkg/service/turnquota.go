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
	"net"
	"sync"

	"github.com/pion/turn/v5"

	"github.com/livekit/protocol/logger"
)

// turnAllocationQuota enforces a per-user cap on concurrent relay allocations on
// the embedded TURN server. Without it, a single authenticated participant can
// reuse its credential across many client 5-tuples during the credential's
// validity window and open one relay allocation (socket + port) per request,
// exhausting the shared relay-port range for every other participant.
//
// The quota is keyed by the user ID returned from TURNAuthHandler.HandleAuth,
// which is the stable participant ID embedded in the signed TURN username. A
// participant cannot forge a different ID without the API secret, so this bounds
// the total relay footprint of any one participant.
//
// Slots are reserved in Allow (which Pion calls before creating an allocation)
// and released in OnDeleted, all under a single lock, so a burst of concurrent
// Allocate requests cannot race past the limit. Reservations are keyed by the
// client source address so that Allocate retransmissions from the same 5-tuple
// are idempotent rather than double-counted.
type turnAllocationQuota struct {
	limit int

	mu    sync.Mutex
	users map[string]map[string]struct{} // userID -> set of active source-address keys
}

func newTURNAllocationQuota(limit int) *turnAllocationQuota {
	return &turnAllocationQuota{
		limit: limit,
		users: make(map[string]map[string]struct{}),
	}
}

func srcAddrKey(srcAddr net.Addr) string {
	if srcAddr == nil {
		return ""
	}
	// include the network so an ip:port reused across UDP and TCP relays is not
	// collapsed into a single slot
	return srcAddr.Network() + "|" + srcAddr.String()
}

// Allow implements turn.QuotaHandler. It returns true if the allocation may
// proceed, reserving a slot for the user, and false (Pion replies 486 Allocation
// Quota Reached) once the user is at its limit.
func (q *turnAllocationQuota) Allow(userID, _ string, srcAddr net.Addr) bool {
	key := srcAddrKey(srcAddr)

	q.mu.Lock()
	defer q.mu.Unlock()

	slots := q.users[userID]
	if _, ok := slots[key]; ok {
		// already reserved for this 5-tuple (retransmit / create race) - idempotent
		return true
	}
	if len(slots) >= q.limit {
		logger.Infow("TURN allocation quota reached",
			"participantID", userID,
			"limit", q.limit,
		)
		return false
	}
	if slots == nil {
		slots = make(map[string]struct{})
		q.users[userID] = slots
	}
	slots[key] = struct{}{}
	return true
}

// OnDeleted implements the allocation-deleted hook, releasing the slot reserved
// in Allow so the user regains capacity once an allocation ends.
func (q *turnAllocationQuota) OnDeleted(srcAddr, _ net.Addr, _, userID, _ string) {
	key := srcAddrKey(srcAddr)

	q.mu.Lock()
	defer q.mu.Unlock()

	slots := q.users[userID]
	if _, ok := slots[key]; !ok {
		return
	}
	delete(slots, key)
	if len(slots) == 0 {
		delete(q.users, userID)
	}
}

// eventHandler returns the Pion EventHandler wired to release quota slots.
func (q *turnAllocationQuota) eventHandler() turn.EventHandler {
	return turn.EventHandler{
		OnAllocationDeleted: q.OnDeleted,
	}
}

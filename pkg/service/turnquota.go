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
	"time"

	"github.com/pion/turn/v5"

	"github.com/livekit/protocol/logger"
)

// defaultTURNReservationTTL bounds how long a reserved-but-unconfirmed
// allocation slot may live. Pion calls the QuotaHandler and then synchronously
// creates the allocation, so a healthy reservation is confirmed within
// microseconds; this generous window only matters if allocation creation fails
// after the quota check (see turnAllocationQuota).
const defaultTURNReservationTTL = 30 * time.Second

// turnAllocationQuota enforces a per-user cap on concurrent relay allocations on
// the embedded TURN server. Without it, a single authenticated participant can
// reuse its credential across many client 5-tuples and open one relay socket/port
// per request, exhausting the shared relay-port range for every other participant.
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
//
// A reservation starts out pending and is confirmed in OnCreated. Pion emits no
// event when an Allocate passes the quota check but then fails to create a relay
// (e.g. the relay-port range is exhausted), so each pending reservation carries a
// reclaim timer that frees the slot after reservationTTL. This keeps a failed
// attempt from occupying a slot forever, which would otherwise let a participant
// lock itself out and grow the tracking map without bound.
type turnAllocationQuota struct {
	limit          int
	reservationTTL time.Duration

	mu    sync.Mutex
	users map[string]map[string]*turnAllocationSlot // userID -> source-address key -> slot
}

type turnAllocationSlot struct {
	confirmed bool
	timer     *time.Timer
}

func newTURNAllocationQuota(limit int) *turnAllocationQuota {
	return &turnAllocationQuota{
		limit:          limit,
		reservationTTL: defaultTURNReservationTTL,
		users:          make(map[string]map[string]*turnAllocationSlot),
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
		slots = make(map[string]*turnAllocationSlot)
		q.users[userID] = slots
	}
	// reserve a pending slot; reclaim it if the allocation is never created so a
	// failed attempt (Pion emits no created/deleted event) cannot hold it forever
	slot := &turnAllocationSlot{}
	slot.timer = time.AfterFunc(q.reservationTTL, func() {
		q.reclaimPending(userID, key)
	})
	slots[key] = slot
	return true
}

// OnCreated confirms a reservation once the relay allocation actually exists,
// cancelling its reclaim timer so a live allocation is never evicted early.
func (q *turnAllocationQuota) OnCreated(srcAddr, _ net.Addr, _, userID, _ string, _ net.Addr, _ int) {
	key := srcAddrKey(srcAddr)

	q.mu.Lock()
	defer q.mu.Unlock()

	slot := q.users[userID][key]
	if slot == nil {
		return
	}
	slot.confirmed = true
	if slot.timer != nil {
		slot.timer.Stop()
		slot.timer = nil
	}
}

// OnDeleted releases the slot reserved in Allow so the user regains capacity once
// an allocation ends.
func (q *turnAllocationQuota) OnDeleted(srcAddr, _ net.Addr, _, userID, _ string) {
	key := srcAddrKey(srcAddr)

	q.mu.Lock()
	defer q.mu.Unlock()

	q.removeLocked(userID, key)
}

// reclaimPending drops a reservation that was never confirmed, freeing a slot
// left behind by an Allocate that passed the quota but failed to create a relay.
func (q *turnAllocationQuota) reclaimPending(userID, key string) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if slot := q.users[userID][key]; slot == nil || slot.confirmed {
		return
	}
	q.removeLocked(userID, key)
}

func (q *turnAllocationQuota) removeLocked(userID, key string) {
	slots := q.users[userID]
	slot := slots[key]
	if slot == nil {
		return
	}
	if slot.timer != nil {
		slot.timer.Stop()
	}
	delete(slots, key)
	if len(slots) == 0 {
		delete(q.users, userID)
	}
}

// eventHandler returns the Pion EventHandler wired to confirm and release quota
// slots as allocations are created and torn down.
func (q *turnAllocationQuota) eventHandler() turn.EventHandler {
	return turn.EventHandler{
		OnAllocationCreated: q.OnCreated,
		OnAllocationDeleted: q.OnDeleted,
	}
}

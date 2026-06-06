// Copyright 2024 LiveKit, Inc.
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
	"math/rand"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/pion/rtcp"
)

// Debug-only simulated publisher ("robot") uplink impairment for the local FlexFEC test
// harness (scripts/flexfec). It models the lossy, higher-RTT first hop a teleop robot sees
// on a wireless/5G link: random loss on inbound RTP (media + RTX + FEC, since real loss
// hits everything) and a one-way delay applied symmetrically to inbound media AND the
// SFU->publisher feedback (NACK), so a NACK->RTX recovery costs a full round trip while a
// normal packet pays one one-way delay. Publisher-side FlexFEC repairs that loss inline at
// the SFU without any round trip -- which is the latency it saves here.
//
// CRITICAL ORDERING NOTE: a real link is ONE pipe -- it delays/loses every packet of every
// SSRC (media, RTX, FEC) together while preserving their global arrival order. The SFU's
// FlexFEC decoder depends on that ordering (it XORs a repair packet against the surviving
// media packets of its block). An earlier design gave each Buffer (media/RTX/FEC SSRC) its
// own delay goroutine; the independent goroutines reordered the streams relative to each
// other and FEC recovery dropped to zero. So this impairment is a single process-global
// FIFO delay line with one drain goroutine: with a constant delay, FIFO dequeue preserves
// the exact arrival order (just time-shifted), and recovery behaves identically to the
// no-delay path.
//
// Controlled by env vars; absent/zero values make this a complete no-op:
//
//	LK_PUB_LOSS      fractional loss on the publisher->SFU path, e.g. 0.03 (3%)
//	LK_PUB_DELAY_MS  one-way publisher<->SFU delay in ms (inbound media + outbound NACK)
//
// (These are the robot-link knobs. The operator/subscriber-side knobs live in the sfu
// package: LK_DOWNLINK_LOSS, LK_DOWNLINK_DELAY_MS, LK_UPLINK_DELAY_MS.)
const (
	envPubLoss    = "LK_PUB_LOSS"
	envPubDelayMs = "LK_PUB_DELAY_MS"

	impairQueueDepth = 8192
)

type delayedInbound struct {
	b         *Buffer
	pkt       []byte
	releaseAt int64 // unix nanos
}

type delayedFeedback struct {
	b         *Buffer
	pkts      []rtcp.Packet
	releaseAt int64 // unix nanos
}

type uplinkImpair struct {
	loss  float64
	delay time.Duration

	rngMu sync.Mutex
	rng   *rand.Rand

	inQueue chan delayedInbound
	fbQueue chan delayedFeedback
}

var (
	globalImpairOnce sync.Once
	globalImpair     *uplinkImpair
)

// getUplinkImpair lazily builds the process-global uplink impairment from the LK_PUB_* env
// vars, or returns nil when none are set. All inbound Buffers share the one instance so the
// delay line preserves global packet ordering across every SSRC (see the note above).
func getUplinkImpair() *uplinkImpair {
	globalImpairOnce.Do(func() {
		loss := parseEnvFloat(envPubLoss)
		delay := time.Duration(parseEnvInt(envPubDelayMs)) * time.Millisecond
		if loss <= 0 && delay <= 0 {
			return
		}
		imp := &uplinkImpair{
			loss:  loss,
			delay: delay,
			rng:   rand.New(rand.NewSource(time.Now().UnixNano())),
		}
		if delay > 0 {
			imp.inQueue = make(chan delayedInbound, impairQueueDepth)
			imp.fbQueue = make(chan delayedFeedback, impairQueueDepth)
			go imp.runInboundDelayLine()
			go imp.runFeedbackDelayLine()
		}
		globalImpair = imp
	})
	return globalImpair
}

// initUplinkImpair attaches the shared uplink impairment to this Buffer (nil unless the
// LK_PUB_* env vars are set). Called once per Buffer from NewBuffer.
func (b *Buffer) initUplinkImpair() {
	b.impair = getUplinkImpair()
	if b.impair != nil && b.logger != nil {
		b.logger.Infow("uplink impairment enabled (debug)", "lossFraction", b.impair.loss, "delay", b.impair.delay)
	}
}

func (u *uplinkImpair) dropInbound() bool {
	if u.loss <= 0 {
		return false
	}
	u.rngMu.Lock()
	r := u.rng.Float64()
	u.rngMu.Unlock()
	return r < u.loss
}

// delayInbound returns true when the packet was deferred onto the shared delay line (caller
// should return immediately); false when no delay is configured and the caller should
// proceed inline.
func (u *uplinkImpair) delayInbound(b *Buffer, pkt []byte) bool {
	if u.delay <= 0 {
		return false
	}
	// pion recycles pkt after Write returns, so copy before deferring.
	cp := make([]byte, len(pkt))
	copy(cp, pkt)
	item := delayedInbound{b: b, pkt: cp, releaseAt: time.Now().Add(u.delay).UnixNano()}
	select {
	case u.inQueue <- item:
	default:
		// Queue full: drop rather than block pion's read loop.
	}
	return true
}

// delayFeedback returns true when feedback was deferred onto the shared delay line; false
// when no delay is configured and the caller should send immediately.
func (u *uplinkImpair) delayFeedback(b *Buffer, pkts []rtcp.Packet) bool {
	if u.delay <= 0 {
		return false
	}
	item := delayedFeedback{b: b, pkts: pkts, releaseAt: time.Now().Add(u.delay).UnixNano()}
	select {
	case u.fbQueue <- item:
	default:
	}
	return true
}

// runInboundDelayLine releases delayed inbound packets, in global FIFO order, into the
// owning buffer's writeNow. A constant delay keeps releaseAt monotonic, so a single
// goroutine preserves the exact arrival order across all SSRCs.
func (u *uplinkImpair) runInboundDelayLine() {
	for item := range u.inQueue {
		if w := item.releaseAt - time.Now().UnixNano(); w > 0 {
			time.Sleep(time.Duration(w))
		}
		_, _ = item.b.writeNow(item.pkt)
	}
}

// runFeedbackDelayLine releases delayed SFU->publisher feedback (NACK/RR) after the one-way
// uplink delay.
func (u *uplinkImpair) runFeedbackDelayLine() {
	for item := range u.fbQueue {
		if w := item.releaseAt - time.Now().UnixNano(); w > 0 {
			time.Sleep(time.Duration(w))
		}
		if cb := item.b.getOnRtcpFeedback(); cb != nil {
			cb(item.pkts)
		}
	}
}

func parseEnvFloat(key string) float64 {
	v, err := strconv.ParseFloat(os.Getenv(key), 64)
	if err != nil {
		return 0
	}
	return v
}

func parseEnvInt(key string) int {
	v, err := strconv.Atoi(os.Getenv(key))
	if err != nil {
		return 0
	}
	return v
}

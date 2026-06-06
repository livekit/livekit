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

package sfu

import (
	"math/rand"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/livekit/protocol/logger"
)

// Debug-only downlink network impairment for the local FlexFEC test harness
// (scripts/flexfec). When the SFU, publisher and subscriber all run on one macOS box,
// media never leaves the host, and pf/dummynet (netem.sh) cannot shape same-host UDP
// because the kernel short-circuits local delivery before the pf OUTPUT hook. To run a
// meaningful FEC-vs-RTX comparison we instead inject loss + one-way delay in software,
// right at the subscriber DownTrack egress.
//
// Controlled entirely by env vars; absent/zero values make wrapping a no-op so this has
// ZERO effect on normal operation:
//
//	LK_DOWNLINK_LOSS      fractional packet loss applied to SFU->subscriber, e.g. 0.01 (1%)
//	LK_DOWNLINK_DELAY_MS  one-way delay added to SFU->subscriber in milliseconds, e.g. 40
//	LK_UPLINK_DELAY_MS    one-way delay added to the subscriber->SFU feedback path (inbound
//	                      RTCP, i.e. NACK/RR/REMB/PLI) in milliseconds, e.g. 40
//
// Set DOWNLINK and UPLINK to the same value to model a symmetric link: a normal frame pays
// one one-way downlink delay, whereas a NACK->RTX recovery pays uplink (delayed NACK) +
// downlink (delayed RTX) = a full RTT. That extra round trip is precisely the latency
// FlexFEC avoids by repairing inline, so a symmetric delay is the fair test of FEC's value.
const (
	envDownlinkLoss    = "LK_DOWNLINK_LOSS"
	envDownlinkDelayMs = "LK_DOWNLINK_DELAY_MS"
	envUplinkDelayMs   = "LK_UPLINK_DELAY_MS"

	// Bounded queue so a stalled subscriber can't grow memory without limit; at ~300
	// pkt/s and tens of ms of delay only a handful are ever in flight.
	impairedQueueDepth = 2048
)

// maybeWrapDownlinkImpairment returns w wrapped with loss/delay impairment when the
// LK_DOWNLINK_* env vars request it, otherwise returns w unchanged.
func maybeWrapDownlinkImpairment(w webrtc.TrackLocalWriter, log logger.Logger) webrtc.TrackLocalWriter {
	loss := parseEnvFloat(envDownlinkLoss)
	delay := time.Duration(parseEnvInt(envDownlinkDelayMs)) * time.Millisecond
	if loss <= 0 && delay <= 0 {
		return w
	}
	if log != nil {
		log.Infow("downlink impairment enabled (debug)", "lossFraction", loss, "delay", delay)
	}
	iw := &impairedWriter{
		inner: w,
		loss:  loss,
		delay: delay,
		rng:   rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	if delay > 0 {
		iw.queue = make(chan delayedWrite, impairedQueueDepth)
		go iw.runDelayLine()
	}
	return iw
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

type delayedWrite struct {
	header    *rtp.Header
	payload   []byte
	raw       []byte
	releaseAt time.Time
}

type impairedWriter struct {
	inner webrtc.TrackLocalWriter
	loss  float64
	delay time.Duration

	mu  sync.Mutex
	rng *rand.Rand

	queue chan delayedWrite
}

func (iw *impairedWriter) drop() bool {
	if iw.loss <= 0 {
		return false
	}
	iw.mu.Lock()
	r := iw.rng.Float64()
	iw.mu.Unlock()
	return r < iw.loss
}

func (iw *impairedWriter) WriteRTP(header *rtp.Header, payload []byte) (int, error) {
	if iw.drop() {
		// Report success so DownTrack stats/sequencing are unaffected; the packet is
		// simply "lost" in the simulated network.
		return header.MarshalSize() + len(payload), nil
	}
	if iw.delay <= 0 {
		return iw.inner.WriteRTP(header, payload)
	}

	// The caller's header/payload come from pooled buffers that are recycled the moment
	// this returns, so everything retained past this call must be copied.
	dw := delayedWrite{
		header:    cloneRTPHeader(header),
		payload:   append([]byte(nil), payload...),
		releaseAt: time.Now().Add(iw.delay),
	}
	n := header.MarshalSize() + len(payload)
	select {
	case iw.queue <- dw:
	default:
		// Queue full (subscriber stalled): drop rather than block the write path.
	}
	return n, nil
}

func (iw *impairedWriter) Write(b []byte) (int, error) {
	if iw.drop() {
		return len(b), nil
	}
	if iw.delay <= 0 {
		return iw.inner.Write(b)
	}
	dw := delayedWrite{
		raw:       append([]byte(nil), b...),
		releaseAt: time.Now().Add(iw.delay),
	}
	select {
	case iw.queue <- dw:
	default:
	}
	return len(b), nil
}

// runDelayLine releases queued writes in order once their release time arrives. A constant
// delay keeps releaseAt monotonic, so a single goroutine preserves packet order.
func (iw *impairedWriter) runDelayLine() {
	for dw := range iw.queue {
		if d := time.Until(dw.releaseAt); d > 0 {
			time.Sleep(d)
		}
		if dw.raw != nil {
			_, _ = iw.inner.Write(dw.raw)
			continue
		}
		_, _ = iw.inner.WriteRTP(dw.header, dw.payload)
	}
}

func cloneRTPHeader(h *rtp.Header) *rtp.Header {
	// rtp.Header.Clone deep-copies CSRC and extension payloads, so the result is safe to
	// retain after the caller recycles its pooled buffers.
	c := h.Clone()
	return &c
}

// maybeDelayInboundRTCP wraps an RTCP packet handler so that inbound RTCP (the
// subscriber->SFU feedback path: NACK/RR/REMB/PLI) is processed LK_UPLINK_DELAY_MS later,
// modelling the uplink leg of the round trip. With this and a matching downlink delay, a
// NACK->RTX recovery pays a full RTT while normal media pays only one one-way delay.
// Returns handler unchanged when LK_UPLINK_DELAY_MS is unset/zero.
func maybeDelayInboundRTCP(handler func([]byte), log logger.Logger) func([]byte) {
	delay := time.Duration(parseEnvInt(envUplinkDelayMs)) * time.Millisecond
	if delay <= 0 {
		return handler
	}
	if log != nil {
		log.Infow("uplink RTCP delay enabled (debug)", "delay", delay)
	}
	queue := make(chan delayedRTCP, impairedQueueDepth)
	go func() {
		for d := range queue {
			if w := time.Until(d.releaseAt); w > 0 {
				time.Sleep(w)
			}
			handler(d.pkt)
		}
	}()
	return func(pkt []byte) {
		// The reader recycles pkt after this returns, so copy before deferring.
		dr := delayedRTCP{
			pkt:       append([]byte(nil), pkt...),
			releaseAt: time.Now().Add(delay),
		}
		select {
		case queue <- dr:
		default:
		}
	}
}

type delayedRTCP struct {
	pkt       []byte
	releaseAt time.Time
}

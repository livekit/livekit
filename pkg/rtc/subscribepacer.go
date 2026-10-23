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
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/livekit/protocol/logger"
)

// SubscribePacer measures, in shadow, how long video subscribes on this node would wait
// if they were paced at each of a set of rates. It never delays a subscribe.
//
// Each rate is a token bucket that refills at that many subscribes per second and holds
// one second of tokens, so a burst of up to one second's worth goes at once and the rest
// waits in line.
type SubscribePacer struct {
	lock    sync.Mutex
	buckets []subscribePaceBucket
}

type subscribePaceBucket struct {
	rateLabel string
	interval  time.Duration // time to refill one token
	burst     time.Duration // how far ahead of now tokens may be taken without waiting
	nextAt    time.Time     // when the next token becomes free, if every earlier subscribe had waited for its token
}

// NewSubscribePacer returns nil when no rates are given, which turns the shadow off.
func NewSubscribePacer(rates []float64) *SubscribePacer {
	rates = slices.Clone(rates)
	slices.Sort(rates)

	var buckets []subscribePaceBucket
	var labels []string
	for i, rate := range rates {
		if rate <= 0 || (i > 0 && rate == rates[i-1]) {
			continue
		}

		interval := time.Duration(float64(time.Second) / rate)
		label := strconv.FormatFloat(rate, 'f', -1, 64)
		buckets = append(buckets, subscribePaceBucket{
			rateLabel: label,
			interval:  interval,
			burst:     max(time.Second-interval, 0),
		})
		labels = append(labels, label)
	}
	if len(buckets) == 0 {
		return nil
	}

	logger.Infow("subscribe pacing shadow enabled", "rates", labels)
	return &SubscribePacer{buckets: buckets}
}

// Delays takes a token from every bucket for a subscribe that starts at now and returns,
// keyed by rate, how long that subscribe would have waited for it.
func (p *SubscribePacer) Delays(now time.Time) map[string]time.Duration {
	if p == nil {
		return nil
	}

	p.lock.Lock()
	defer p.lock.Unlock()

	delays := make(map[string]time.Duration, len(p.buckets))
	for i := range p.buckets {
		b := &p.buckets[i]
		takeAt := b.nextAt
		if takeAt.Before(now) {
			takeAt = now
		}
		delays[b.rateLabel] = max(takeAt.Sub(now)-b.burst, 0)
		b.nextAt = takeAt.Add(b.interval)
	}
	return delays
}

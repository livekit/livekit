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

package sfu

import (
	"sync"

	"github.com/pion/webrtc/v4"
	"go.uber.org/atomic"
)

// RTXBudgetConfig caps the bytes a down track retransmits in response to NACKs.
type RTXBudgetConfig struct {
	Audio RTXBudgetLimit `yaml:"audio,omitempty"`
	Video RTXBudgetLimit `yaml:"video,omitempty"`
}

type RTXBudgetLimit struct {
	// retransmitted bytes allowed, as a fraction of the media bytes sent; 0 turns the cap off
	MaxRatio float64 `yaml:"max_ratio,omitempty"`
	// retransmitted bytes allowed at once, above the steady ratio; at least one full packet
	BurstBytes int `yaml:"burst_bytes,omitempty"`
}

// the budget never holds less than one full packet
const minRTXBurstBytes = 1500

// The bursts cover about 250 ms of total loss: a 2 Mbps video stream and a 128 kbps audio stream.
var DefaultRTXBudgetConfig = RTXBudgetConfig{
	Audio: RTXBudgetLimit{BurstBytes: 4_000},
	Video: RTXBudgetLimit{BurstBytes: 64_000},
}

func (c RTXBudgetConfig) forKind(kind webrtc.RTPCodecType) RTXBudgetLimit {
	if kind == webrtc.RTPCodecTypeAudio {
		return c.Audio
	}
	return c.Video
}

// rtxBudget is a token bucket filled by media bytes sent and drained by retransmitted bytes.
// A retransmission is allowed while the bucket is above zero and may overdraw it, so the
// check needs no packet size and runs before the packet is read from the publisher's buffer.
// A nil budget allows every retransmission.
type rtxBudget struct {
	ratio float64
	burst float64

	mediaBytes atomic.Uint64

	lock           sync.Mutex
	tokens         float64
	mediaBytesSeen uint64

	// NACKed packets not retransmitted because the budget was empty
	numCapped atomic.Uint64
}

func newRTXBudget(config RTXBudgetLimit) *rtxBudget {
	if config.MaxRatio <= 0 {
		return nil
	}
	burst := float64(max(config.BurstBytes, minRTXBurstBytes))
	return &rtxBudget{
		ratio:  config.MaxRatio,
		burst:  burst,
		tokens: burst,
	}
}

func (b *rtxBudget) onMediaSent(size int) {
	if b == nil {
		return
	}
	b.mediaBytes.Add(uint64(size))
}

// allow reports whether the budget has room for another retransmission.
func (b *rtxBudget) allow() bool {
	if b == nil {
		return true
	}

	b.lock.Lock()
	defer b.lock.Unlock()

	mediaBytes := b.mediaBytes.Load()
	b.tokens = min(b.burst, b.tokens+b.ratio*float64(mediaBytes-b.mediaBytesSeen))
	b.mediaBytesSeen = mediaBytes
	return b.tokens > 0
}

func (b *rtxBudget) spend(size int) {
	if b == nil {
		return
	}

	b.lock.Lock()
	b.tokens -= float64(size)
	b.lock.Unlock()
}

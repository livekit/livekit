// Copyright 2023 LiveKit, Inc.
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
	"fmt"
	"hash/fnv"
	"math/rand"
	"runtime"
	"sync"
	"testing"

	"github.com/gammazero/workerpool"
	"github.com/stretchr/testify/assert"
	"go.uber.org/atomic"
)

func TestWebRTCReceiver_OnCloseHandler(t *testing.T) {
	type args struct {
		fn func()
	}
	tests := []struct {
		name string
		args args
	}{
		{
			name: "Must set on close handler function",
			args: args{
				fn: func() {},
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			w := &WebRTCReceiver{}
			w.OnCloseHandler(tt.args.fn)
			assert.NotNil(t, w.onCloseHandler)
		})
	}
}

func BenchmarkWriteRTP(b *testing.B) {
	cases := []int{1, 2, 5, 10, 100, 250, 500}
	workers := runtime.NumCPU()
	wp := workerpool.New(workers)
	for _, c := range cases {
		// fills each bucket with a max of 50, i.e. []int{50, 50} for c=100
		fill := make([]int, 0)
		for i := 50; ; i += 50 {
			if i > c {
				fill = append(fill, c%50)
				break
			}

			fill = append(fill, 50)
			if i == c {
				break
			}
		}

		// splits c into numCPU buckets, i.e. []int{9, 9, 9, 9, 8, 8, 8, 8, 8, 8, 8, 8} for 12 cpus and c=100
		split := make([]int, workers)
		for i := range split {
			split[i] = c / workers
		}
		for i := 0; i < c%workers; i++ {
			split[i]++
		}

		b.Run(fmt.Sprintf("%d-Downtracks/Control", c), func(b *testing.B) {
			benchmarkNoPool(b, c)
		})
		b.Run(fmt.Sprintf("%d-Downtracks/Pool(Fill)", c), func(b *testing.B) {
			benchmarkPool(b, wp, fill)
		})
		b.Run(fmt.Sprintf("%d-Downtracks/Pool(Hash)", c), func(b *testing.B) {
			benchmarkPool(b, wp, split)
		})
		b.Run(fmt.Sprintf("%d-Downtracks/Goroutines", c), func(b *testing.B) {
			benchmarkGoroutine(b, split)
		})
		b.Run(fmt.Sprintf("%d-Downtracks/LoadBalanced", c), func(b *testing.B) {
			benchmarkLoadBalanced(b, workers, 2, c)
		})
		b.Run(fmt.Sprintf("%d-Downtracks/LBPool", c), func(b *testing.B) {
			benchmarkLoadBalancedPool(b, wp, workers, 2, c)
		})
	}
}

func benchmarkNoPool(b *testing.B, downTracks int) {
	for i := 0; i < b.N; i++ {
		for dt := 0; dt < downTracks; dt++ {
			writeRTP()
		}
	}
}

func benchmarkPool(b *testing.B, wp *workerpool.WorkerPool, buckets []int) {
	for i := 0; i < b.N; i++ {
		var wg sync.WaitGroup
		for j := range buckets {
			downTracks := buckets[j]
			if downTracks == 0 {
				continue
			}
			wg.Add(1)
			wp.Submit(func() {
				defer wg.Done()
				for dt := 0; dt < downTracks; dt++ {
					writeRTP()
				}
			})
		}
		wg.Wait()
	}
}

func benchmarkGoroutine(b *testing.B, buckets []int) {
	for i := 0; i < b.N; i++ {
		var wg sync.WaitGroup
		for j := range buckets {
			downTracks := buckets[j]
			if downTracks == 0 {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				for dt := 0; dt < downTracks; dt++ {
					writeRTP()
				}
			}()
		}
		wg.Wait()
	}
}

func benchmarkLoadBalanced(b *testing.B, numProcs, step, downTracks int) {
	for i := 0; i < b.N; i++ {
		start := atomic.NewUint64(0)
		step := uint64(step)
		end := uint64(downTracks)

		var wg sync.WaitGroup
		wg.Add(numProcs)
		for p := 0; p < numProcs; p++ {
			go func() {
				defer wg.Done()
				for {
					n := start.Add(step)
					if n >= end+step {
						return
					}

					for i := n - step; i < n && i < end; i++ {
						writeRTP()
					}
				}
			}()
		}
		wg.Wait()
	}
}

func benchmarkLoadBalancedPool(b *testing.B, wp *workerpool.WorkerPool, numProcs, step, downTracks int) {
	for i := 0; i < b.N; i++ {
		start := atomic.NewUint64(0)
		step := uint64(step)
		end := uint64(downTracks)

		var wg sync.WaitGroup
		wg.Add(numProcs)
		for p := 0; p < numProcs; p++ {
			wp.Submit(func() {
				defer wg.Done()
				for {
					n := start.Add(step)
					if n >= end+step {
						return
					}

					for i := n - step; i < n && i < end; i++ {
						writeRTP()
					}
				}
			})
		}
		wg.Wait()
	}
}

func writeRTP() {
	s := []byte("simulate some work")
	stop := 1900 + rand.Intn(200)
	for j := 0; j < stop; j++ {
		h := fnv.New128()
		s = h.Sum(s)
	}
}

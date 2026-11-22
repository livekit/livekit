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

package endpoint

import (
	"context"
	"io"
	"sync"
)

// Tunnels tracks hijacked connections so a drain can close them.
type Tunnels struct {
	mu       sync.Mutex
	live     map[io.Closer]struct{}
	idle     chan struct{} // closed when live empties; nil while nobody waits
	draining chan struct{}
	once     sync.Once
}

func NewTunnels() *Tunnels {
	return &Tunnels{live: make(map[io.Closer]struct{}), draining: make(chan struct{})}
}

// Add tracks c. It reports false once a drain has begun: c is tracked
// regardless, so the drain still waits for it, but the caller must close it.
func (s *Tunnels) Add(c io.Closer) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.live[c] = struct{}{}
	return !s.IsDraining()
}

func (s *Tunnels) Remove(c io.Closer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.live, c)
	if len(s.live) == 0 && s.idle != nil {
		close(s.idle)
		s.idle = nil
	}
}

func (s *Tunnels) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.live)
}

// Draining is closed once a drain has begun.
func (s *Tunnels) Draining() <-chan struct{} {
	return s.draining
}

func (s *Tunnels) IsDraining() bool {
	select {
	case <-s.draining:
		return true
	default:
		return false
	}
}

// Drain lets the tracked connections end until ctx is done, then closes the
// rest. It returns once none remain and is safe to call repeatedly.
func (s *Tunnels) Drain(ctx context.Context) {
	s.once.Do(func() { close(s.draining) })
	select {
	case <-s.idleCh():
		return
	case <-ctx.Done():
	}
	s.mu.Lock()
	open := make([]io.Closer, 0, len(s.live))
	for c := range s.live {
		open = append(open, c)
	}
	s.mu.Unlock()
	for _, c := range open {
		_ = c.Close()
	}
	<-s.idleCh()
}

func (s *Tunnels) idleCh() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.live) == 0 {
		c := make(chan struct{})
		close(c)
		return c
	}
	if s.idle == nil {
		s.idle = make(chan struct{})
	}
	return s.idle
}

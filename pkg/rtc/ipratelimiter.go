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

package rtc

import (
	"sync"

	"golang.org/x/time/rate"
)

// IPRateLimiter manages rate limiters keyed by IP address.
// It's safe for concurrent use and cleans up entries when all participants
// from an IP disconnect (via reference counting).
type IPRateLimiter struct {
	mu        sync.Mutex
	limiters  map[string]*ipLimiterEntry
	rateLimit rate.Limit
	burst     int
}

type ipLimiterEntry struct {
	limiter  *rate.Limiter
	refCount int
}

// NewIPRateLimiter creates a new IP-based rate limiter.
// rateLimit is the number of events per second, burst is the maximum burst size.
// Returns nil if rateLimit <= 0 (disabled).
func NewIPRateLimiter(rateLimit float64, burst int) *IPRateLimiter {
	if rateLimit <= 0 {
		return nil
	}
	if burst <= 0 {
		burst = 1
	}
	return &IPRateLimiter{
		limiters:  make(map[string]*ipLimiterEntry),
		rateLimit: rate.Limit(rateLimit),
		burst:     burst,
	}
}

// Allow checks if an event from the given IP should be allowed.
// Returns true if the event is allowed, false if rate limited.
func (l *IPRateLimiter) Allow(ip string) bool {
	if l == nil || ip == "" {
		return true
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	entry, exists := l.limiters[ip]
	if !exists {
		// Entry doesn't exist yet - this shouldn't happen in normal flow
		// since AddRef is called before any data messages, but allow it
		return true
	}

	return entry.limiter.Allow()
}

// AddRef increments the reference count for an IP (called when a participant connects).
// Creates the rate limiter entry if it doesn't exist.
func (l *IPRateLimiter) AddRef(ip string) {
	if l == nil || ip == "" {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	entry, exists := l.limiters[ip]
	if !exists {
		entry = &ipLimiterEntry{
			limiter:  rate.NewLimiter(l.rateLimit, l.burst),
			refCount: 0,
		}
		l.limiters[ip] = entry
	}
	entry.refCount++
}

// RemoveRef decrements the reference count for an IP (called when a participant disconnects).
// When the count reaches zero, the limiter entry is removed.
func (l *IPRateLimiter) RemoveRef(ip string) {
	if l == nil || ip == "" {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	entry, exists := l.limiters[ip]
	if !exists {
		return
	}

	entry.refCount--
	if entry.refCount <= 0 {
		delete(l.limiters, ip)
	}
}

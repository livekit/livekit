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

package testutils

import (
	"context"
	"testing"
	"time"
)

var (
	ConnectTimeout = 30 * time.Second
)

func WithTimeout(t *testing.T, f func() string, timeouts ...time.Duration) {
	timeout := ConnectTimeout
	if len(timeouts) > 0 {
		timeout = timeouts[0]
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	lastErr := ""
	for {
		select {
		case <-ctx.Done():
			if lastErr != "" {
				t.Fatalf("did not reach expected state after %v: %s", timeout, lastErr)
			}
		case <-time.After(10 * time.Millisecond):
			lastErr = f()
			if lastErr == "" {
				return
			}
		}
	}
}

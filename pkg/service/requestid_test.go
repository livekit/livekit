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
	"context"
	"strings"
	"testing"
)

func TestDeterministicID(t *testing.T) {
	const prefix = "EG_"

	// Same request id -> same id: this is what makes an SDK-retried create dedup.
	a := DeterministicID(prefix, "req_123")
	if b := DeterministicID(prefix, "req_123"); a != b {
		t.Fatalf("not stable for same request id: %q != %q", a, b)
	}
	if !strings.HasPrefix(a, prefix) {
		t.Fatalf("id %q missing prefix %q", a, prefix)
	}

	// Different request ids -> different ids.
	if c := DeterministicID(prefix, "req_456"); c == a {
		t.Fatalf("different request ids produced the same id: %q", c)
	}

	// Different prefixes -> different ids (so e.g. a room and identity derived
	// from the same request id don't collide).
	if d := DeterministicID("IN_", "req_123"); d == a {
		t.Fatalf("different prefixes produced the same id: %q", d)
	}

	// Empty request id -> random (each call differs), still prefixed. Preserves
	// non-idempotent behavior when the client sent no request id.
	r1 := DeterministicID(prefix, "")
	r2 := DeterministicID(prefix, "")
	if r1 == r2 {
		t.Fatalf("empty request id should produce random ids, got equal: %q", r1)
	}
	if !strings.HasPrefix(r1, prefix) {
		t.Fatalf("random id %q missing prefix %q", r1, prefix)
	}
}

func TestSaltRequestID(t *testing.T) {
	// A salted seed derives a distinct-but-stable id (e.g. an ingress stream key
	// vs its ingress id), so the two don't collide.
	id := DeterministicID("IN_", "req_123")
	sk := DeterministicID("", saltRequestID("req_123", "streamkey"))
	if id == sk {
		t.Fatalf("salted derivation collided with unsalted: %q", id)
	}
	if sk2 := DeterministicID("", saltRequestID("req_123", "streamkey")); sk2 != sk {
		t.Fatalf("salted derivation not stable: %q != %q", sk, sk2)
	}
	// Empty in -> empty out, so DeterministicID still falls back to random.
	if got := saltRequestID("", "streamkey"); got != "" {
		t.Fatalf("saltRequestID(\"\", ...) = %q, want empty", got)
	}
}

func TestRequestID(t *testing.T) {
	if got := RequestID(context.Background()); got != "" {
		t.Fatalf("RequestID on bare context = %q, want empty", got)
	}
	ctx := WithRequestID(context.Background(), "req_abc")
	if got := RequestID(ctx); got != "req_abc" {
		t.Fatalf("RequestID = %q, want req_abc", got)
	}
	// An empty id is a no-op (the client sent none).
	if got := RequestID(WithRequestID(context.Background(), "")); got != "" {
		t.Fatalf("WithRequestID(\"\") should be a no-op, got %q", got)
	}
}

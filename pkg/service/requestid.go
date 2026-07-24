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

	"github.com/livekit/protocol/utils/guid"
)

// RequestIDAttribute is the participant attribute stamped with the request id
// for calls that join a room before dialing. A
// retry finds the existing participant with a matching value and skips the dial.
const RequestIDAttribute = "lk.request_id"

type requestIDKey struct{}

// WithRequestID stores the client idempotency id on the context and propagates
// it to downstream services via outgoing metadata. A no-op when id is empty.
func WithRequestID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, requestIDKey{}, id)
}

func RequestID(ctx context.Context) string {
	if id, ok := ctx.Value(requestIDKey{}).(string); ok && id != "" {
		return id
	}
	return ""
}

// DeterministicID derives a stable resource id from the request id — retries of the same logical call
// yield the same id and dedup at the store. With no request id it falls back to a random id.
func DeterministicID(prefix, requestID string) string {
	if requestID == "" {
		return guid.New(prefix)
	}
	return guid.Hash(prefix, []byte(requestID))
}

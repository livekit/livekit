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

package service_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/service"
)

// readAllHandler mimics the way a Twirp handler consumes the whole request body
// before doing anything else. It records how much it managed to read and whether
// the read failed (e.g. because the body limit was exceeded).
type readAllHandler struct {
	bytesRead int
	readErr   error
	called    bool
}

func (h *readAllHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.called = true
	if r.Body == nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	b, err := io.ReadAll(r.Body)
	h.bytesRead = len(b)
	h.readErr = err
	if err != nil {
		// a real decoder surfaces this as a 4xx/5xx; emulate that
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func TestRequestBodyLimiter(t *testing.T) {
	const limit = 1024

	t.Run("rejects oversized declared Content-Length before decoding", func(t *testing.T) {
		l := service.NewRequestBodyLimiter(limit)
		handler := &readAllHandler{}

		body := strings.NewReader(strings.Repeat("a", limit*4))
		r := httptest.NewRequest(http.MethodPost, "/twirp/livekit.Egress/StartRoomCompositeEgress", body)
		require.EqualValues(t, limit*4, r.ContentLength)
		w := httptest.NewRecorder()

		l.ServeHTTP(w, r, handler.ServeHTTP)

		require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
		// the body must never be handed to the decoder
		require.False(t, handler.called)
	})

	t.Run("bounds body when Content-Length is absent/dishonest", func(t *testing.T) {
		l := service.NewRequestBodyLimiter(limit)
		handler := &readAllHandler{}

		body := strings.NewReader(strings.Repeat("a", limit*4))
		r := httptest.NewRequest(http.MethodPost, "/twirp/livekit.Egress/StartRoomCompositeEgress", body)
		// simulate chunked encoding / unknown length
		r.ContentLength = -1
		w := httptest.NewRecorder()

		l.ServeHTTP(w, r, handler.ServeHTTP)

		// the decoder was invoked but could not read more than the limit
		require.True(t, handler.called)
		require.Error(t, handler.readErr)
		require.LessOrEqual(t, handler.bytesRead, limit)
	})

	t.Run("allows request within limit", func(t *testing.T) {
		l := service.NewRequestBodyLimiter(limit)
		handler := &readAllHandler{}

		payload := strings.Repeat("a", limit/2)
		r := httptest.NewRequest(http.MethodPost, "/twirp/livekit.Egress/StartRoomCompositeEgress", strings.NewReader(payload))
		w := httptest.NewRecorder()

		l.ServeHTTP(w, r, handler.ServeHTTP)

		require.Equal(t, http.StatusOK, w.Code)
		require.True(t, handler.called)
		require.NoError(t, handler.readErr)
		require.Equal(t, len(payload), handler.bytesRead)
	})

	t.Run("disabled when limit is non-positive", func(t *testing.T) {
		l := service.NewRequestBodyLimiter(0)
		handler := &readAllHandler{}

		payload := strings.Repeat("a", limit*8)
		r := httptest.NewRequest(http.MethodPost, "/twirp/livekit.Egress/StartRoomCompositeEgress", strings.NewReader(payload))
		w := httptest.NewRecorder()

		l.ServeHTTP(w, r, handler.ServeHTTP)

		require.Equal(t, http.StatusOK, w.Code)
		require.NoError(t, handler.readErr)
		require.Equal(t, len(payload), handler.bytesRead)
	})

	t.Run("passes through nil body", func(t *testing.T) {
		l := service.NewRequestBodyLimiter(limit)
		handler := &readAllHandler{}

		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Body = nil
		w := httptest.NewRecorder()

		l.ServeHTTP(w, r, handler.ServeHTTP)

		require.True(t, handler.called)
	})
}

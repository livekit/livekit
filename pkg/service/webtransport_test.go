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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/urfave/negroni/v3"
)

// An upgrade type-asserts the ResponseWriter to http3.Settingser and
// http3.HTTPStreamer without checking, so it must reach the one net/http handed
// in. Every negroni chain wraps it.
func TestUnwrapResponseWriterReachesTheOriginal(t *testing.T) {
	original := httptest.NewRecorder()

	var seen http.ResponseWriter
	chain := negroni.New()
	chain.UseHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		seen = w
	}))
	chain.ServeHTTP(original, httptest.NewRequest(http.MethodGet, "/agent", nil))

	_, wrapped := seen.(interface{ Unwrap() http.ResponseWriter })
	require.True(t, wrapped, "negroni no longer wraps; this test no longer covers anything")
	require.Same(t, original, unwrapResponseWriter(seen))
}

func TestUnwrapResponseWriterPassesThroughUnwrapped(t *testing.T) {
	original := httptest.NewRecorder()
	require.Same(t, original, unwrapResponseWriter(original))
}

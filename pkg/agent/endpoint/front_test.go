// Copyright 2026 LiveKit, Inc.

package endpoint

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

func fallbackFront(t *testing.T, fb Fallback, withWorker bool) *Front {
	reg := NewRegistry()
	if withWorker {
		m, err := ParseManifest([]*livekit.AgentHttp_AgentEndpoint{
			{Path: "/known", Methods: []string{"GET"}, Public: true},
		})
		require.NoError(t, err)
		r := &Registration{WorkerID: "w1", APIKey: "proj", AgentName: "a", Deployment: "d", Manifest: m}
		r.SetSession(&fakeSession{})
		require.NoError(t, reg.Register(r))
	}
	f := NewFront(reg, func(*http.Request) (string, bool) { return "proj", true }, logger.GetLogger())
	if fb != nil {
		f = f.WithFallback(fb)
	}
	return f
}

func serveFront(f *Front, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	f.ServeHTTP(w, httptest.NewRequest(http.MethodGet, PathPrefix+"a/d"+path, nil))
	return w
}

// a path no local worker matches hands off to the fallback, which is given the
// resolved identity; when the fallback serves, the front writes nothing itself.
func TestFrontFallbackFires(t *testing.T) {
	var got *FallbackRequest
	f := fallbackFront(t, func(w http.ResponseWriter, _ *http.Request, fr *FallbackRequest) bool {
		got = fr
		w.WriteHeader(http.StatusTeapot) // stands in for a relayed response
		return true
	}, true)

	w := serveFront(f, "/unknown")
	require.Equal(t, http.StatusTeapot, w.Code)
	require.NotNil(t, got)
	require.Equal(t, "proj", got.APIKey)
	require.True(t, got.Authenticated)
	require.Equal(t, "a", got.AgentName)
	require.Equal(t, "d", got.Deployment)
}

// a declined fallback with a local worker present falls through to the front's
// own 404 for the unmatched path.
func TestFrontFallbackDeclinedMapsStatus(t *testing.T) {
	f := fallbackFront(t, func(http.ResponseWriter, *http.Request, *FallbackRequest) bool { return false }, true)
	require.Equal(t, http.StatusNotFound, serveFront(f, "/unknown").Code)
}

// a declined fallback with no local worker for the deployment falls through to
// 503.
func TestFrontFallbackDeclinedNoCandidates(t *testing.T) {
	f := fallbackFront(t, func(http.ResponseWriter, *http.Request, *FallbackRequest) bool { return false }, false)
	require.Equal(t, http.StatusServiceUnavailable, serveFront(f, "/unknown").Code)
}

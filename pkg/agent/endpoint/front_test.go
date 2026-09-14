// Copyright 2026 LiveKit, Inc.

package endpoint

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

func TestRequestIDAcceptsOrRefuses(t *testing.T) {
	atBound := strings.Repeat("x", maxRequestIDLen)
	cases := []struct {
		name   string
		values []string
		want   string
		ok     bool
	}{
		{name: "absent mints one", ok: true},
		{name: "uuid", values: []string{"f81d4fae-7dec-11d0-a765-00a0c91e6bf6"}, want: "f81d4fae-7dec-11d0-a765-00a0c91e6bf6", ok: true},
		{name: "base64url", values: []string{"a-B_c9=="}, want: "a-B_c9==", ok: true},
		{name: "at the length bound", values: []string{atBound}, want: atBound, ok: true},
		{name: "past the length bound", values: []string{atBound + "x"}},
		{name: "empty value", values: []string{""}},
		{name: "newline", values: []string{"ab\ncd"}},
		{name: "space", values: []string{"ab cd"}},
		{name: "non ascii", values: []string{"abc\u00e9"}},
		{name: "two values", values: []string{"a", "b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/x", nil)
			for _, v := range tc.values {
				r.Header.Add("X-Request-Id", v)
			}
			got, ok := requestID(r)
			require.Equal(t, tc.ok, ok)
			switch {
			case !tc.ok:
				require.Empty(t, got, "a refused token must not reach the worker or the logs")
			case tc.want != "":
				require.Equal(t, tc.want, got, "a client token is never rewritten")
			default:
				require.True(t, strings.HasPrefix(got, "AER_"))
			}
		})
	}
}

// a token the front cannot honor is refused before any routing or dispatch, so
// the caller learns to retry under one it will honor instead of silently losing
// idempotence to a substitute.
func TestFrontRefusesInvalidRequestID(t *testing.T) {
	f := fallbackFront(t, nil, true)

	serve := func(token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, PathPrefix+"a/d/known", nil)
		r.Header.Set("X-Request-Id", token)
		w := httptest.NewRecorder()
		f.ServeHTTP(w, r)
		return w
	}

	require.Equal(t, http.StatusBadRequest, serve(strings.Repeat("x", maxRequestIDLen+1)).Code)
	// same route, acceptable token: the request reaches dispatch (and fails
	// there for want of a stream), so the 400 above came from the token alone
	require.Equal(t, http.StatusServiceUnavailable, serve("f81d4fae-7dec").Code)
}

// the preamble is built once, so a retry must not hand the worker a budget
// already spent.
func TestRefreshTimeoutTracksRemainingBudget(t *testing.T) {
	newAttempt := func(ctx context.Context) *attempt {
		a := &attempt{req: httptest.NewRequest(http.MethodGet, "/x", nil).WithContext(ctx)}
		a.preamble = a.newPreamble()
		return a
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	a := newAttempt(ctx)
	a.refreshTimeout()
	first := a.preamble.GetTimeoutMs()
	require.NotZero(t, first)

	time.Sleep(25 * time.Millisecond)
	a.refreshTimeout()
	require.Less(t, a.preamble.GetTimeoutMs(), first)

	// no deadline: 0 is the proto's "no deadline"
	none := newAttempt(context.Background())
	none.refreshTimeout()
	require.Zero(t, none.preamble.GetTimeoutMs())

	// an expired budget is not "no deadline"
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()
	spent := newAttempt(expired)
	spent.refreshTimeout()
	require.EqualValues(t, 1, spent.preamble.GetTimeoutMs())
}

// Copyright 2026 LiveKit, Inc.

package endpoint

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

// grantedTo resolves every request to apiKey with full access.
func grantedTo(apiKey string) AccessResolver {
	return func(*http.Request, string, string) Access {
		return Access{APIKey: apiKey, Level: AccessGranted}
	}
}

func fallbackFront(t *testing.T, fb Fallback, withWorker bool) *Front {
	reg := NewRegistry()
	if withWorker {
		m, err := ParseManifest([]*livekit.AgentHttp_AgentEndpoint{
			{Path: "/known", Methods: []string{"GET"}, Public: true},
		})
		require.NoError(t, err)
		r := NewRegistration(RegistrationParams{WorkerID: "w1", APIKey: "proj", AgentName: "a", Deployment: "d", Manifest: m, Session: &fakeSession{}})
		reg.Register(r)
	}
	return NewFront(FrontParams{
		Registry:      reg,
		ResolveAccess: grantedTo("proj"),
		Logger:        logger.GetLogger(),
		Fallback:      fb,
	})
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
	require.Equal(t, AccessGranted, got.Level)
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

// accessFront registers one worker serving a public and a non-public route, and
// resolves every request to the given access.
func accessFront(t *testing.T, a Access, fb Fallback) *Front {
	reg := NewRegistry()
	m, err := ParseManifest([]*livekit.AgentHttp_AgentEndpoint{
		{Path: "/pub", Methods: []string{"GET"}, Public: true},
		{Path: "/private", Methods: []string{"GET"}, Public: false},
	})
	require.NoError(t, err)
	r := NewRegistration(RegistrationParams{WorkerID: "w1", APIKey: "proj", AgentName: "a", Deployment: "d", Manifest: m, Session: &fakeSession{}})
	reg.Register(r)

	return NewFront(FrontParams{
		Registry:      reg,
		ResolveAccess: func(*http.Request, string, string) Access { return a },
		Logger:        logger.GetLogger(),
		Fallback:      fb,
	})
}

// fakeSession opens no stream, so a request that clears authorization reaches 503.
func TestFrontPrivateRouteAccessMapping(t *testing.T) {
	anonymous := Access{APIKey: "proj", Level: AccessNone}
	credentialed := Access{APIKey: "proj", Level: AccessCredentialed}
	granted := Access{APIKey: "proj", Level: AccessGranted}

	t.Run("anonymous is challenged", func(t *testing.T) {
		w := serveFront(accessFront(t, anonymous, nil), "/private")
		require.Equal(t, http.StatusUnauthorized, w.Code)
		require.Equal(t, "Bearer", w.Header().Get("WWW-Authenticate"))
	})
	t.Run("credential without the grant is refused, not challenged", func(t *testing.T) {
		w := serveFront(accessFront(t, credentialed, nil), "/private")
		require.Equal(t, http.StatusForbidden, w.Code)
		require.Empty(t, w.Header().Get("WWW-Authenticate"))
	})
	t.Run("granted passes authorization", func(t *testing.T) {
		require.Equal(t, http.StatusServiceUnavailable, serveFront(accessFront(t, granted, nil), "/private").Code)
	})
	t.Run("a credential without the grant keeps public access", func(t *testing.T) {
		require.Equal(t, http.StatusServiceUnavailable, serveFront(accessFront(t, credentialed, nil), "/pub").Code)
	})
	t.Run("anonymous keeps public access", func(t *testing.T) {
		require.Equal(t, http.StatusServiceUnavailable, serveFront(accessFront(t, anonymous, nil), "/pub").Code)
	})
}

// the slash-normalized form of a private route is still private.
func TestFrontDeniedAppliesToNormalizedPath(t *testing.T) {
	f := accessFront(t, Access{APIKey: "proj", Level: AccessCredentialed}, nil)
	require.Equal(t, http.StatusForbidden, serveFront(f, "/private/").Code)
}

// another node's worker may declare the same path public.
func TestFrontDeniedStillRelays(t *testing.T) {
	var got *FallbackRequest
	f := accessFront(t, Access{APIKey: "proj", Level: AccessCredentialed}, func(w http.ResponseWriter, _ *http.Request, fr *FallbackRequest) bool {
		got = fr
		w.WriteHeader(http.StatusTeapot)
		return true
	})

	require.Equal(t, http.StatusTeapot, serveFront(f, "/private").Code)
	require.NotNil(t, got)
	require.Equal(t, AccessCredentialed, got.Level)
}

// the split runs before decoding, so a name or route param may carry any byte
// a percent-encoded segment can hold.
func TestSplitEndpointPath(t *testing.T) {
	cases := []struct {
		name       string
		target     string
		agentName  string
		deployment string
		path       string
		escPath    string
		err        error
	}{
		{name: "plain", target: "/agents/a/d/x", agentName: "a", deployment: "d", path: "/x", escPath: "/x"},
		{name: "no tail", target: "/agents/a/d", agentName: "a", deployment: "d", path: "/", escPath: "/"},
		{name: "spaces", target: "/agents/LODHA%20Vayam%20Agent/d/x", agentName: "LODHA Vayam Agent", deployment: "d", path: "/x", escPath: "/x"},
		{name: "colon and star", target: "/agents/prod%3A%2A/d/x", agentName: "prod:*", deployment: "d", path: "/x", escPath: "/x"},
		{name: "at sign", target: "/agents/charlie%40v1.42.0/d/x", agentName: "charlie@v1.42.0", deployment: "d", path: "/x", escPath: "/x"},
		{name: "brackets", target: "/agents/Nathan%20%5BElara%5D/d/x", agentName: "Nathan [Elara]", deployment: "d", path: "/x", escPath: "/x"},
		{name: "slash in name", target: "/agents/a%2Fb/d/x", agentName: "a/b", deployment: "d", path: "/x", escPath: "/x"},
		{name: "non ascii raw", target: "/agents/agent-ü/d/x", agentName: "agent-ü", deployment: "d", path: "/x", escPath: "/x"},
		{name: "non ascii encoded", target: "/agents/agent-%C3%BC/d/x", agentName: "agent-ü", deployment: "d", path: "/x", escPath: "/x"},
		{name: "past the old 64 byte cap", target: "/agents/" + strings.Repeat("n", 77) + "/d/x", agentName: strings.Repeat("n", 77), deployment: "d", path: "/x", escPath: "/x"},
		{name: "deployment encoded", target: "/agents/a/prod%20us/x", agentName: "a", deployment: "prod us", path: "/x", escPath: "/x"},

		// escPath keeps the client's encoding; path is what the manifest matches
		{name: "encoded tail", target: "/agents/a/d/files/a%2Fb", agentName: "a", deployment: "d", path: "/files/a/b", escPath: "/files/a%2Fb"},
		{name: "escaped percent in tail", target: "/agents/a/d/%2541", agentName: "a", deployment: "d", path: "/%41", escPath: "/%2541"},

		// "_" addresses the unnamed agent in either form
		{name: "bare underscore is unnamed", target: "/agents/_/d/x", agentName: "", deployment: "d", path: "/x", escPath: "/x"},
		{name: "encoded underscore is unnamed", target: "/agents/%5F/d/x", agentName: "", deployment: "d", path: "/x", escPath: "/x"},
		{name: "double encoded underscore is a name", target: "/agents/%255F/d/x", agentName: "%5F", deployment: "d", path: "/x", escPath: "/x"},

		{name: "not an endpoint path", target: "/other/x", err: errNotEndpointPath},
		{name: "no deployment segment", target: "/agents/a", err: errNotEndpointPath},
		{name: "empty name", target: "/agents//d/x", err: errNotEndpointPath},
		{name: "empty deployment", target: "/agents/a//x", err: errNotEndpointPath},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u, err := url.ParseRequestURI(c.target)
			require.NoError(t, err)

			ep, err := splitEndpointPath(u)
			if c.err != nil {
				require.ErrorIs(t, err, c.err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, c.agentName, ep.agentName)
			require.Equal(t, c.deployment, ep.deployment)
			require.Equal(t, c.path, ep.path)
			require.Equal(t, c.escPath, ep.escPath)
		})
	}
}

// a trailing %2F decodes to a slash without being a separator, so the
// trailing-slash alternate does not apply to it.
func TestFrontSlashAlternateIgnoresEncodedSlash(t *testing.T) {
	f := fallbackFront(t, nil, true)

	// 503 is dispatch reached: the fake session opens no stream
	require.Equal(t, http.StatusServiceUnavailable, serveFront(f, "/known/").Code)
	require.Equal(t, http.StatusNotFound, serveFront(f, "/known%2F").Code)
}

func TestIsReservedAgentName(t *testing.T) {
	for _, n := range []string{"_", ".", ".."} {
		require.True(t, IsReservedAgentName(n), n)
	}
	for _, n := range []string{"", "a", "_x", "x_", "...", "LODHA Vayam Agent", "%5F"} {
		require.False(t, IsReservedAgentName(n), n)
	}
}

// matching runs before anything sizes the request head, so the split is where
// an over-long path is refused.
func TestSplitEndpointPathLength(t *testing.T) {
	long := PathPrefix + "a/d/" + strings.Repeat("x", MaxPathLength)
	_, err := splitEndpointPath(&url.URL{Path: long, RawPath: long})
	require.ErrorIs(t, err, errPathTooLong)

	f := fallbackFront(t, nil, true)
	require.Equal(t, http.StatusRequestURITooLong, serveFront(f, "/"+strings.Repeat("x", MaxPathLength)).Code)

	ok := PathPrefix + "a/d/" + strings.Repeat("x", MaxPathLength-4)
	_, err = splitEndpointPath(&url.URL{Path: ok, RawPath: ok})
	require.NoError(t, err)
}

// budgetFront registers a worker whose routes are ambiguous enough that the
// matcher gives up deciding.
func budgetFront(t *testing.T, a Access) *Front {
	reg := NewRegistry()
	m, err := ParseManifest([]*livekit.AgentHttp_AgentEndpoint{
		{Path: "/{a}{b}{c}{d}x", Methods: []string{"GET"}, Public: true},
	})
	require.NoError(t, err)
	require.Len(t, m.Ambiguous(), 1)
	reg.Register(NewRegistration(RegistrationParams{
		WorkerID: "w1", APIKey: "proj", AgentName: "a", Deployment: "d",
		Manifest: m, Session: &fakeSession{},
	}))
	return NewFront(FrontParams{
		Registry:      reg,
		ResolveAccess: func(*http.Request, string, string) Access { return a },
		Logger:        logger.GetLogger(),
	})
}

// A table too ambiguous to decide forwards to the worker. No route is decided,
// so its Public flag is unknown and only a grant clears the request.
func TestFrontOverBudgetForwardsWithAGrant(t *testing.T) {
	long := "/" + strings.Repeat("a", 512)

	f := budgetFront(t, Access{APIKey: "proj", Level: AccessGranted})
	// 503 is dispatch reached: the fake session opens no stream
	require.Equal(t, http.StatusServiceUnavailable, serveFront(f, long).Code)

	f = budgetFront(t, Access{APIKey: "proj", Level: AccessCredentialed})
	require.Equal(t, http.StatusForbidden, serveFront(f, long).Code)

	f = budgetFront(t, Access{APIKey: "proj", Level: AccessNone})
	require.Equal(t, http.StatusUnauthorized, serveFront(f, long).Code)

	// a path the same table decides normally is unaffected
	f = budgetFront(t, Access{APIKey: "proj", Level: AccessNone})
	require.Equal(t, http.StatusNotFound, serveFront(f, "/ab").Code)
}

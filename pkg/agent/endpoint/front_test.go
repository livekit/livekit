// Copyright 2026 LiveKit, Inc.

package endpoint

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/agent/endpoint/wire"
)

// grantedTo resolves every request to scope with full access.
func grantedTo(scope *Scope) AccessResolver {
	return func(*http.Request, string, string) (Access, bool) {
		return Access{Scope: scope, Level: AccessGranted}, true
	}
}

// resolveTo resolves every request to a fixed access.
func resolveTo(a Access) AccessResolver {
	return func(*http.Request, string, string) (Access, bool) { return a, true }
}

func fallbackFront(t *testing.T, fb Fallback, withWorker bool) *Front {
	g, s := NewRegistry(), NewScope(logger.GetLogger())
	if withWorker {
		m, err := ParseManifest([]*livekit.AgentHttp_AgentEndpoint{
			{Path: "/known", Methods: []string{"GET"}, Public: true},
		})
		require.NoError(t, err)
		g.Register(s, NewRegistration(RegistrationParams{WorkerID: "w1", Manifest: m, Session: &fakeSession{}}))
	}
	return NewFront(FrontParams{
		ResolveAccess: resolveTo(Access{Scope: s, Fallback: fb, Level: AccessGranted}),
		Logger:        logger.GetLogger(),
	})
}

func serveFront(f *Front, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	f.ServeHTTP(w, httptest.NewRequest(http.MethodGet, PathPrefix+"a/d"+path, nil))
	return w
}

// a path no local worker matches hands off to the fallback the resolver curried,
// which is given the caller's access level; when the fallback serves, the front
// writes nothing itself.
func TestFrontFallbackFires(t *testing.T) {
	got := AccessLevel(-1)
	f := fallbackFront(t, func(w http.ResponseWriter, _ *http.Request, level AccessLevel) bool {
		got = level
		w.WriteHeader(http.StatusTeapot) // stands in for a relayed response
		return true
	}, true)

	w := serveFront(f, "/unknown")
	require.Equal(t, http.StatusTeapot, w.Code)
	require.Equal(t, AccessGranted, got)
}

// a declined fallback with a local worker present falls through to the front's
// own 404 for the unmatched path.
func TestFrontFallbackDeclinedMapsStatus(t *testing.T) {
	f := fallbackFront(t, func(http.ResponseWriter, *http.Request, AccessLevel) bool { return false }, true)
	require.Equal(t, http.StatusNotFound, serveFront(f, "/unknown").Code)
}

// a declined fallback with no local worker for the deployment falls through to
// 503.
func TestFrontFallbackDeclinedNoCandidates(t *testing.T) {
	f := fallbackFront(t, func(http.ResponseWriter, *http.Request, AccessLevel) bool { return false }, false)
	require.Equal(t, http.StatusServiceUnavailable, serveFront(f, "/unknown").Code)
}

// a resolver that cannot place the request at all challenges, and never reaches
// a scope. This is the path that used to ride an empty api key.
func TestFrontUnresolvedAccessIsChallenged(t *testing.T) {
	f := NewFront(FrontParams{
		ResolveAccess: func(*http.Request, string, string) (Access, bool) { return Access{}, false },
		Logger:        logger.GetLogger(),
	})
	w := serveFront(f, "/anything")
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Equal(t, "Bearer", w.Header().Get("WWW-Authenticate"))
}

// a resolved caller whose deployment no worker holds here, with no fallback, is
// 503 - distinct from the 401 above, and no longer distinguished by a string.
func TestFrontResolvedButNoScopeIsUnavailable(t *testing.T) {
	f := NewFront(FrontParams{
		ResolveAccess: resolveTo(Access{Level: AccessGranted}),
		Logger:        logger.GetLogger(),
	})
	require.Equal(t, http.StatusServiceUnavailable, serveFront(f, "/anything").Code)
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
func accessFront(t *testing.T, level AccessLevel, fb Fallback) *Front {
	g, s := NewRegistry(), NewScope(logger.GetLogger())
	m, err := ParseManifest([]*livekit.AgentHttp_AgentEndpoint{
		{Path: "/pub", Methods: []string{"GET"}, Public: true},
		{Path: "/private", Methods: []string{"GET"}, Public: false},
	})
	require.NoError(t, err)
	g.Register(s, NewRegistration(RegistrationParams{WorkerID: "w1", Manifest: m, Session: &fakeSession{}}))

	return NewFront(FrontParams{
		ResolveAccess: resolveTo(Access{Scope: s, Fallback: fb, Level: level}),
		Logger:        logger.GetLogger(),
	})
}

// fakeSession opens no stream, so a request that clears authorization reaches 503.
func TestFrontPrivateRouteAccessMapping(t *testing.T) {
	anonymous, credentialed, granted := AccessNone, AccessCredentialed, AccessGranted

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
	f := accessFront(t, AccessCredentialed, nil)
	require.Equal(t, http.StatusForbidden, serveFront(f, "/private/").Code)
}

// another node's worker may declare the same path public.
func TestFrontDeniedStillRelays(t *testing.T) {
	got := AccessLevel(-1)
	f := accessFront(t, AccessCredentialed, func(w http.ResponseWriter, _ *http.Request, level AccessLevel) bool {
		got = level
		w.WriteHeader(http.StatusTeapot)
		return true
	})

	require.Equal(t, http.StatusTeapot, serveFront(f, "/private").Code)
	require.Equal(t, AccessCredentialed, got)
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
func budgetFront(t *testing.T, level AccessLevel) *Front {
	g, s := NewRegistry(), NewScope(logger.GetLogger())
	m, err := ParseManifest([]*livekit.AgentHttp_AgentEndpoint{
		{Path: "/{a}{b}{c}{d}x", Methods: []string{"GET"}, Public: true},
	})
	require.NoError(t, err)
	require.Len(t, m.Ambiguous(), 1)
	g.Register(s, NewRegistration(RegistrationParams{
		WorkerID: "w1", Manifest: m, Session: &fakeSession{},
	}))
	return NewFront(FrontParams{
		ResolveAccess: resolveTo(Access{Scope: s, Level: level}),
		Logger:        logger.GetLogger(),
	})
}

// A table too ambiguous to decide forwards to the worker. No route is decided,
// so its Public flag is unknown and only a grant clears the request.
func TestFrontOverBudgetForwardsWithAGrant(t *testing.T) {
	long := "/" + strings.Repeat("a", 512)

	f := budgetFront(t, AccessGranted)
	// 503 is dispatch reached: the fake session opens no stream
	require.Equal(t, http.StatusServiceUnavailable, serveFront(f, long).Code)

	f = budgetFront(t, AccessCredentialed)
	require.Equal(t, http.StatusForbidden, serveFront(f, long).Code)

	f = budgetFront(t, AccessNone)
	require.Equal(t, http.StatusUnauthorized, serveFront(f, long).Code)

	// a path the same table decides normally is unaffected
	f = budgetFront(t, AccessNone)
	require.Equal(t, http.StatusNotFound, serveFront(f, "/ab").Code)
}

func TestMergeWorkerHeaders(t *testing.T) {
	sharedVary := []string{"Origin"}
	dst := http.Header{
		"Access-Control-Allow-Origin":   {"https://server.example"},
		"Access-Control-Expose-Headers": {"*"},
		"Vary":                          sharedVary,
		"Set-Cookie":                    {"a=1"},
	}
	MergeWorkerHeaders(dst, http.Header{
		"Access-Control-Allow-Origin": {"https://app.example"},
		"Vary":                        {"Accept-Encoding"},
		"Set-Cookie":                  {"b=2"},
		"Link":                        {"</a>; rel=preload", "</b>; rel=preload"},
	})

	require.Equal(t, []string{"https://app.example"}, dst.Values("Access-Control-Allow-Origin"))
	require.Equal(t, []string{"*"}, dst.Values("Access-Control-Expose-Headers"), "a key the worker left alone keeps the server's value")
	require.Equal(t, []string{"Accept-Encoding"}, dst.Values("Vary"))
	require.Equal(t, []string{"a=1", "b=2"}, dst.Values("Set-Cookie"))
	require.Equal(t, []string{"</a>; rel=preload", "</b>; rel=preload"}, dst.Values("Link"))
	require.Equal(t, []string{"Origin"}, sharedVary, "a slice the middleware shares must not be written through")
}

// textExchange is what the scripted worker saw of one request.
type textExchange struct {
	pre    *livekit.AgentHttp_StreamPreamble
	target string
	header http.Header
	body   string
}

// textFront serves one scripted worker declaring a text-kind route next to a
// plain one, and reports each exchange it served. A nil ResolveAccess grants
// every request.
func textFront(t *testing.T, params FrontParams) (*httptest.Server, <-chan textExchange) {
	t.Helper()
	m, err := ParseManifest([]*livekit.AgentHttp_AgentEndpoint{
		{Path: "/a2a", Kind: livekit.AgentHttp_AEK_TEXT, Methods: []string{"POST"}, Public: true},
		{Path: "/plain", Methods: []string{"POST"}, Public: true},
	})
	require.NoError(t, err)

	seen := make(chan textExchange, 1)
	g, s := NewRegistry(), NewScope(logger.GetLogger())
	g.Register(s, NewRegistration(RegistrationParams{WorkerID: "w1", Manifest: m, Session: &scriptedSession{
		script: func(r io.Reader, w *workerSide) {
			defer w.Close()
			pre, err := wire.ReadPreamble(r)
			if err != nil {
				return
			}
			req, err := http.ReadRequest(bufio.NewReader(r))
			if err != nil {
				return
			}
			body, _ := io.ReadAll(req.Body)
			seen <- textExchange{pre: pre, target: req.RequestURI, header: req.Header, body: string(body)}
			w.WriteString("HTTP/1.1 204 No Content\r\nContent-Length: 0\r\n\r\n")
		},
	}}))
	if params.ResolveAccess == nil {
		params.ResolveAccess = grantedTo(s)
	} else {
		params.ResolveAccess = withScope(params.ResolveAccess, s)
	}
	params.Logger = logger.GetLogger()
	ts := httptest.NewServer(NewFront(params))
	t.Cleanup(ts.Close)
	return ts, seen
}

func postText(t *testing.T, ts *httptest.Server, path, body string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+PathPrefix+"a/d"+path, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("X-Channel", "sms")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
}

// the preamble carries the kind the picked worker declared the route with, and
// with no resolver installed a text route is forwarded unchanged: no thread,
// whole body.
func TestFrontTextRouteWithoutResolverPassesThrough(t *testing.T) {
	ts, seen := textFront(t, FrontParams{})
	body := strings.Repeat("x", 3*DefaultThreadPeek)

	postText(t, ts, "/a2a", body)
	got := <-seen
	require.Equal(t, livekit.AgentHttp_AEK_TEXT, got.pre.GetKind())
	require.Empty(t, got.pre.GetThreadId())
	require.Equal(t, body, got.body)

	postText(t, ts, "/plain", body)
	got = <-seen
	require.Equal(t, livekit.AgentHttp_AEK_HTTP, got.pre.GetKind())
	require.Equal(t, body, got.body)
}

// a resolver sees the request head and a bounded prefix of the body; its id
// reaches the text-kind worker in the preamble, and the worker still gets the
// whole body.
func TestFrontThreadResolverPlacesTextRoute(t *testing.T) {
	type call struct {
		method, path, channel, agent, deployment string
		peek                                     string
	}
	calls := make(chan call, 1)
	ts, seen := textFront(t, FrontParams{
		ThreadPeek: 8,
		ResolveThread: func(r *http.Request, agentName, deployment string, body []byte) (string, bool) {
			calls <- call{r.Method, r.URL.Path, r.Header.Get("X-Channel"), agentName, deployment, string(body)}
			return "AT_1", true
		},
	})
	body := `{"jsonrpc":"2.0","params":` + strings.Repeat("x", 2*DefaultThreadPeek) + `}`

	postText(t, ts, "/a2a", body)
	got := <-seen
	require.Equal(t, livekit.AgentHttp_AEK_TEXT, got.pre.GetKind())
	require.Equal(t, "AT_1", got.pre.GetThreadId())
	require.Equal(t, body, got.body, "the peek must not take bytes from the worker")

	c := <-calls
	require.Equal(t, call{http.MethodPost, PathPrefix + "a/d/a2a", "sms", "a", "d", body[:8]}, c)

	// a plain route is not the front's to read: the resolver never runs
	postText(t, ts, "/plain", body)
	got = <-seen
	require.Equal(t, livekit.AgentHttp_AEK_HTTP, got.pre.GetKind())
	require.Empty(t, got.pre.GetThreadId())
	require.Equal(t, body, got.body)
	require.Empty(t, calls)
}

// a resolver that places nothing leaves the thread to the worker.
func TestFrontThreadResolverDeclines(t *testing.T) {
	ts, seen := textFront(t, FrontParams{
		ResolveThread: func(*http.Request, string, string, []byte) (string, bool) { return "", false },
	})
	postText(t, ts, "/a2a", "hello")
	got := <-seen
	require.Equal(t, livekit.AgentHttp_AEK_TEXT, got.pre.GetKind())
	require.Empty(t, got.pre.GetThreadId())
	require.Equal(t, "hello", got.body)
}

// a body shorter than the peek bound is handed over whole, with nothing lost to
// the read-ahead; an absent body peeks nothing.
func TestFrontThreadPeekShortBody(t *testing.T) {
	peeks := make(chan string, 1)
	ts, seen := textFront(t, FrontParams{
		ResolveThread: func(_ *http.Request, _, _ string, body []byte) (string, bool) {
			peeks <- string(body)
			return "AT_2", true
		},
	})

	postText(t, ts, "/a2a", "short")
	require.Equal(t, "short", <-peeks)
	got := <-seen
	require.Equal(t, "AT_2", got.pre.GetThreadId())
	require.Equal(t, "short", got.body)

	postText(t, ts, "/a2a", "")
	require.Empty(t, <-peeks)
	got = <-seen
	require.Equal(t, "AT_2", got.pre.GetThreadId())
	require.Empty(t, got.body)
}

const testAPIKey, testAPISecret = "APIfront", "front-secret-front-secret-front-secret"

// verifiedAccess resolves access the way the server's middleware and resolver
// do together: a LiveKit token from the bearer, or from access_token on the
// query when there is no bearer, is verified and its claims decide the level
// and the thread grant. No token is anonymous.
func verifiedAccess(agentName, deployment string) AccessResolver {
	return func(r *http.Request, _, _ string) (Access, bool) {
		raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if r.Header.Get("Authorization") == "" {
			raw = r.URL.Query().Get("access_token")
		}
		if raw == "" {
			return Access{}, true
		}
		v, err := auth.ParseAPIToken(raw)
		if err != nil {
			return Access{}, false
		}
		_, claims, err := v.Verify(testAPISecret)
		if err != nil {
			return Access{}, false
		}
		level := AccessCredentialed
		if claims.AgentEndpoint.Allows(agentName, deployment) {
			level = AccessGranted
		}
		return Access{Level: level, ThreadGrant: claims.AgentThread}, true
	}
}

// withScope places whatever resolve decides in scope.
func withScope(resolve AccessResolver, scope *Scope) AccessResolver {
	return func(r *http.Request, agentName, deployment string) (Access, bool) {
		a, ok := resolve(r, agentName, deployment)
		a.Scope = scope
		return a, ok
	}
}

func mintToken(t *testing.T, grant *auth.AgentThreadGrant) string {
	t.Helper()
	tok, err := auth.NewAccessToken(testAPIKey, testAPISecret).
		SetIdentity("caller").
		SetAgentEndpointGrant(&auth.AgentEndpointGrant{Call: true}).
		SetAgentThreadGrant(grant).
		ToJWT()
	require.NoError(t, err)
	return tok
}

// the verified token's agentThread claim reaches the worker in the preamble, on
// a text route and a plain one alike, while the token itself does not.
func TestFrontThreadGrantTravelsInPreamble(t *testing.T) {
	ts, seen := textFront(t, FrontParams{ResolveAccess: verifiedAccess("a", "d")})
	grant := &auth.AgentThreadGrant{
		AgentName: "a", Sub: "demo-user", ThreadIDs: []string{"AT_1", "AT_2"},
		List: true, Create: true, Send: true, Read: true,
	}
	tok := mintToken(t, grant)

	for _, path := range []string{"/a2a", "/plain"} {
		req, err := http.NewRequest(http.MethodPost, ts.URL+PathPrefix+"a/d"+path, strings.NewReader("hello"))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		resp.Body.Close()
		require.Equal(t, http.StatusNoContent, resp.StatusCode)

		got := <-seen
		require.True(t, got.pre.GetAuthorized())
		require.Empty(t, got.header.Get("Authorization"), "the token must never reach the worker")
		require.Equal(t, "a", got.pre.GetThreadGrant().GetAgentName())
		require.Equal(t, "demo-user", got.pre.GetThreadGrant().GetSub())
		require.Equal(t, []string{"AT_1", "AT_2"}, got.pre.GetThreadGrant().GetThreadIds())
		require.True(t, got.pre.GetThreadGrant().GetList())
		require.True(t, got.pre.GetThreadGrant().GetCreate())
		require.True(t, got.pre.GetThreadGrant().GetSend())
		require.True(t, got.pre.GetThreadGrant().GetRead())
		require.False(t, got.pre.GetThreadGrant().GetDelete())
	}
}

// a browser WebSocket cannot set headers, so the token may ride the query as
// access_token: it is verified the same way, the grant travels in the preamble,
// and the query the worker sees no longer carries it.
func TestFrontThreadGrantFromQueryToken(t *testing.T) {
	ts, seen := textFront(t, FrontParams{ResolveAccess: verifiedAccess("a", "d")})
	tok := mintToken(t, &auth.AgentThreadGrant{AgentName: "a", Sub: "demo-user", Send: true})

	req, err := http.NewRequest(http.MethodPost, ts.URL+PathPrefix+"a/d/a2a?access_token="+tok+"&keep=1", strings.NewReader("hello"))
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	got := <-seen
	require.Equal(t, "demo-user", got.pre.GetThreadGrant().GetSub())
	require.Equal(t, "a", got.pre.GetThreadGrant().GetAgentName())
	require.True(t, got.pre.GetThreadGrant().GetSend())
	require.Empty(t, got.header.Get("Authorization"))
	require.Equal(t, "/a2a?keep=1", got.target, "the query token is stripped like the bearer")
}

// the grant is the node's to assert: a client-set X-LiveKit-Thread-Grant is
// dropped, and a token with no agentThread claim leaves the field unset.
func TestFrontDropsForgedThreadGrantHeader(t *testing.T) {
	ts, seen := textFront(t, FrontParams{ResolveAccess: verifiedAccess("a", "d")})
	tok := mintToken(t, nil)

	req, err := http.NewRequest(http.MethodPost, ts.URL+PathPrefix+"a/d/a2a", strings.NewReader("hello"))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set(threadGrantHeader, `{"agentName":"a","sub":"someone-else","send":true}`)
	req.Header.Set("X-Keep", "1")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	got := <-seen
	require.True(t, got.pre.GetAuthorized())
	require.Nil(t, got.pre.GetThreadGrant(), "a project token with no agentThread claim grants no thread")
	require.Empty(t, got.header.Values(threadGrantHeader), "a client cannot forge the grant")
	require.Equal(t, "1", got.header.Get("X-Keep"), "other headers still pass")
	require.Empty(t, got.header.Get("Authorization"))
}

// a grant that lists endpoints opens only those: the first path segment under
// the deployment is the endpoint, and one outside the list is forbidden before
// any worker sees the request. A grant with no endpoints opens every one.
func TestFrontThreadGrantEndpointsScopeTheRoute(t *testing.T) {
	ts, seen := textFront(t, FrontParams{ResolveAccess: verifiedAccess("a", "d")})
	scoped := mintToken(t, &auth.AgentThreadGrant{AgentName: "a", Sub: "demo-user", Send: true, Endpoints: []string{"front-desk"}})
	open := mintToken(t, &auth.AgentThreadGrant{AgentName: "a", Sub: "demo-user", Send: true})

	post := func(tok string) int {
		req, err := http.NewRequest(http.MethodPost, ts.URL+PathPrefix+"a/d/a2a", strings.NewReader("hello"))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		resp.Body.Close()
		return resp.StatusCode
	}
	require.Equal(t, http.StatusForbidden, post(scoped), "a2a is not among the grant's endpoints")
	require.Equal(t, http.StatusNoContent, post(open))
	got := <-seen
	require.Empty(t, got.pre.GetThreadGrant().GetEndpoints())
}

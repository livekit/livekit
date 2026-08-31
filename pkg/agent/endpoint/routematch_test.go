// Copyright 2026 LiveKit, Inc.

package endpoint

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// idx builds an ExactIndex whose routes all serve GET; the path-shape tests
// below match with GET via the matches helper. Method-awareness has its own test.
func idx(t *testing.T, routes ...string) *ExactIndex {
	x := NewExactIndex()
	for _, r := range routes {
		require.NoError(t, x.Add([]string{http.MethodGet}, r), r)
	}
	return x
}

// matches is a GET-method shorthand so the path-shape assertions read cleanly.
func matches(idx PrefixIndex, path string) bool {
	return Matches(idx, http.MethodGet, path)
}

func TestRouteMatchStaticAndParams(t *testing.T) {
	x := idx(t, "/sms", "/orders/{id}/items", "/orders/{id}/list", "/health")

	// static
	require.True(t, matches(x, "/sms"))
	require.True(t, matches(x, "/health"))
	require.False(t, matches(x, "/unknown"))

	// single-segment params, and the two sub-routes are distinguished
	require.True(t, matches(x, "/orders/42/items"))
	require.True(t, matches(x, "/orders/abc/list"))
	require.False(t, matches(x, "/orders/42/refund")) // neither sub-route exists
	require.False(t, matches(x, "/orders/42"))        // prefix only, not a full route
	require.False(t, matches(x, "/orders/42/items/x"))

	// trailing slash matches the same shape (worker 307s)
	require.True(t, matches(x, "/orders/42/items/"))
}

func TestRouteMatchTypedParams(t *testing.T) {
	x := idx(t, "/orders/{id:int}", "/u/{u:uuid}")

	require.True(t, matches(x, "/orders/42"))
	require.False(t, matches(x, "/orders/abc"))  // not an int -> edge miss, no relay
	require.False(t, matches(x, "/orders/3.14")) // float is not int

	uuid := "550e8400-e29b-41d4-a716-446655440000"
	require.True(t, matches(x, "/u/"+uuid))
	require.True(t, matches(x, "/u/"+"550e8400e29b41d4a716446655440000")) // hyphenless
	require.False(t, matches(x, "/u/not-a-uuid"))
}

func TestRouteMatchUUIDvsStr(t *testing.T) {
	// a uuid value is also a valid str; whichever the route declared wins
	uuid := "550e8400-e29b-41d4-a716-446655440000"

	strOnly := idx(t, "/x/{v}")                   // str
	require.True(t, matches(strOnly, "/x/"+uuid)) // uuid value matches a str route
	require.True(t, matches(strOnly, "/x/anything"))

	uuidOnly := idx(t, "/x/{v:uuid}")
	require.True(t, matches(uuidOnly, "/x/"+uuid))
	require.False(t, matches(uuidOnly, "/x/anything")) // non-uuid never matches a uuid route
}

func TestRouteMatchGlob(t *testing.T) {
	x := idx(t, "/files/{rest:path}", "/static")

	require.True(t, matches(x, "/files/a"))
	require.True(t, matches(x, "/files/a/b/c/d.txt")) // spans segments
	// /files with no trailing slash is a tolerated FALSE POSITIVE (Starlette
	// requires the separating slash); the worker returns the real status
	require.True(t, matches(x, "/files"))
	require.False(t, matches(x, "/other/a/b"))
	require.True(t, matches(x, "/static"))
}

// a path convertor that is not the last segment must still match (the glob is
// treated as terminal, over-approximating the suffix)
func TestRouteMatchNonTerminalGlob(t *testing.T) {
	x := idx(t, "/files/{rest:path}/edit")
	require.True(t, matches(x, "/files/a/b/edit"))
	require.True(t, matches(x, "/files/a/edit"))
	// suffix is over-approximated: a path under /files matches even without
	// /edit (the worker's router returns the real 404)
	require.True(t, matches(x, "/files/a/b"))
	require.False(t, matches(x, "/other/a/edit"))
}

// a path convertor mixed with literal text in a segment spans slashes and must
// not be narrowed to a single-segment str
func TestRouteMatchMixedGlob(t *testing.T) {
	x := idx(t, "/files/pre{rest:path}")
	require.True(t, matches(x, "/files/prea"))
	require.True(t, matches(x, "/files/prea/b/c")) // spans segments (would drop under str)
	require.False(t, matches(x, "/other/x"))
}

func TestRouteMatchHeterogeneousShapes(t *testing.T) {
	// two different workers' route sets, same path shape, different types
	ints := idx(t, "/item/{id:int}")
	strs := idx(t, "/item/{slug}")

	require.True(t, matches(ints, "/item/42"))
	require.False(t, matches(ints, "/item/foo")) // routes only to the str worker
	require.True(t, matches(strs, "/item/foo"))
	require.True(t, matches(strs, "/item/42")) // str accepts a numeric slug too
}

func TestRouteMatchLiteralBeatsWildcardCoexist(t *testing.T) {
	// a literal segment and a param at the same position coexist
	x := idx(t, "/users/me", "/users/{id:int}")
	require.True(t, matches(x, "/users/me"))
	require.True(t, matches(x, "/users/42"))
	require.False(t, matches(x, "/users/abc")) // neither: not "me", not an int
}

// the terminal is method-qualified: the SAME path shape served under different
// methods (e.g. two workers, POST vs PUT) matches only for the declared method,
// so the cross-node presence filter is method-aware.
func TestRouteMatchMethodAware(t *testing.T) {
	postOnly := NewExactIndex()
	require.NoError(t, postOnly.Add([]string{http.MethodPost}, "/test"))
	putOnly := NewExactIndex()
	require.NoError(t, putOnly.Add([]string{http.MethodPut}, "/test"))

	require.True(t, Matches(postOnly, http.MethodPost, "/test"))
	require.False(t, Matches(postOnly, http.MethodPut, "/test"))
	require.True(t, Matches(putOnly, http.MethodPut, "/test"))
	require.False(t, Matches(putOnly, http.MethodPost, "/test"))

	// a route declaring several methods matches each, and only those
	multi := NewExactIndex()
	require.NoError(t, multi.Add([]string{http.MethodGet, http.MethodHead}, "/thing/{id:int}"))
	require.True(t, Matches(multi, http.MethodGet, "/thing/5"))
	require.True(t, Matches(multi, http.MethodHead, "/thing/5"))
	require.False(t, Matches(multi, http.MethodDelete, "/thing/5"))
	require.False(t, Matches(multi, http.MethodGet, "/thing/abc")) // still shape-checked
}

func TestRouteMatchDepthCapFallsBackToRelay(t *testing.T) {
	x := idx(t, "/a/{b}")
	deep := "/" // build a > maxMatchDepth path
	for i := 0; i < maxMatchDepth+5; i++ {
		deep += "x/"
	}
	require.True(t, matches(x, deep)) // over cap -> relay and let the worker decide
}

// depth enforcement lives in the manifest layer (ParseManifest), not in the
// index: Add canonicalizes any depth without error, so the two layers stay
// decoupled.
func TestRouteDepthNotEnforcedAtAdd(t *testing.T) {
	x := NewExactIndex()
	long := "/"
	for i := 0; i < MaxRouteDepth+2; i++ {
		long += "s/"
	}
	require.NoError(t, x.Add([]string{http.MethodGet}, long))
}

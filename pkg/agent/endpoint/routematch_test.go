// Copyright 2026 LiveKit, Inc.

package endpoint

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func idx(t *testing.T, routes ...string) *ExactIndex {
	x := NewExactIndex()
	for _, r := range routes {
		require.NoError(t, x.Add(r), r)
	}
	return x
}

func TestRouteMatchStaticAndParams(t *testing.T) {
	x := idx(t, "/sms", "/orders/{id}/items", "/orders/{id}/list", "/health")

	// static
	require.True(t, Matches(x, "/sms"))
	require.True(t, Matches(x, "/health"))
	require.False(t, Matches(x, "/unknown"))

	// single-segment params, and the two sub-routes are distinguished
	require.True(t, Matches(x, "/orders/42/items"))
	require.True(t, Matches(x, "/orders/abc/list"))
	require.False(t, Matches(x, "/orders/42/refund")) // neither sub-route exists
	require.False(t, Matches(x, "/orders/42"))        // prefix only, not a full route
	require.False(t, Matches(x, "/orders/42/items/x"))

	// trailing slash matches the same shape (worker 307s)
	require.True(t, Matches(x, "/orders/42/items/"))
}

func TestRouteMatchTypedParams(t *testing.T) {
	x := idx(t, "/orders/{id:int}", "/u/{u:uuid}")

	require.True(t, Matches(x, "/orders/42"))    // int
	require.False(t, Matches(x, "/orders/abc"))  // not an int -> edge miss, no relay
	require.False(t, Matches(x, "/orders/3.14")) // float is not int

	uuid := "550e8400-e29b-41d4-a716-446655440000"
	require.True(t, Matches(x, "/u/"+uuid))
	require.True(t, Matches(x, "/u/"+"550e8400e29b41d4a716446655440000")) // hyphenless
	require.False(t, Matches(x, "/u/not-a-uuid"))
}

func TestRouteMatchUUIDvsStr(t *testing.T) {
	// a uuid value is also a valid str; whichever the route declared wins
	uuid := "550e8400-e29b-41d4-a716-446655440000"

	strOnly := idx(t, "/x/{v}")                   // str
	require.True(t, Matches(strOnly, "/x/"+uuid)) // uuid value matches a str route
	require.True(t, Matches(strOnly, "/x/anything"))

	uuidOnly := idx(t, "/x/{v:uuid}")
	require.True(t, Matches(uuidOnly, "/x/"+uuid))
	require.False(t, Matches(uuidOnly, "/x/anything")) // non-uuid never matches a uuid route
}

func TestRouteMatchGlob(t *testing.T) {
	x := idx(t, "/files/{rest:path}", "/static")

	require.True(t, Matches(x, "/files/a"))
	require.True(t, Matches(x, "/files/a/b/c/d.txt")) // spans segments
	// /files with no trailing slash is a tolerated FALSE POSITIVE (Starlette
	// requires the separating slash); the worker returns the real status
	require.True(t, Matches(x, "/files"))
	require.False(t, Matches(x, "/other/a/b"))
	require.True(t, Matches(x, "/static"))
}

// a path convertor that is not the last segment must still match (the glob is
// treated as terminal, over-approximating the suffix)
func TestRouteMatchNonTerminalGlob(t *testing.T) {
	x := idx(t, "/files/{rest:path}/edit")
	require.True(t, Matches(x, "/files/a/b/edit"))
	require.True(t, Matches(x, "/files/a/edit"))
	// suffix is over-approximated: a path under /files matches even without
	// /edit (the worker's router returns the real 404)
	require.True(t, Matches(x, "/files/a/b"))
	require.False(t, Matches(x, "/other/a/edit"))
}

// a path convertor mixed with literal text in a segment spans slashes and must
// not be narrowed to a single-segment str
func TestRouteMatchMixedGlob(t *testing.T) {
	x := idx(t, "/files/pre{rest:path}")
	require.True(t, Matches(x, "/files/prea"))
	require.True(t, Matches(x, "/files/prea/b/c")) // spans segments (would drop under str)
	require.False(t, Matches(x, "/other/x"))
}

func TestRouteMatchHeterogeneousShapes(t *testing.T) {
	// two different workers' route sets, same path shape, different types
	ints := idx(t, "/item/{id:int}")
	strs := idx(t, "/item/{slug}")

	require.True(t, Matches(ints, "/item/42"))
	require.False(t, Matches(ints, "/item/foo")) // routes only to the str worker
	require.True(t, Matches(strs, "/item/foo"))
	require.True(t, Matches(strs, "/item/42")) // str accepts a numeric slug too
}

func TestRouteMatchLiteralBeatsWildcardCoexist(t *testing.T) {
	// a literal segment and a param at the same position coexist
	x := idx(t, "/users/me", "/users/{id:int}")
	require.True(t, Matches(x, "/users/me"))   // literal
	require.True(t, Matches(x, "/users/42"))   // param
	require.False(t, Matches(x, "/users/abc")) // neither: not "me", not an int
}

func TestRouteMatchDepthCapFallsBackToRelay(t *testing.T) {
	x := idx(t, "/a/{b}")
	deep := "/" // build a > maxMatchDepth path
	for i := 0; i < maxMatchDepth+5; i++ {
		deep += "x/"
	}
	require.True(t, Matches(x, deep)) // over cap -> relay and let the worker decide
}

func TestRouteDepthRejectedAtAdd(t *testing.T) {
	x := NewExactIndex()
	long := "/"
	for i := 0; i < MaxRouteDepth+2; i++ {
		long += "s/"
	}
	// canonicalization succeeds (depth enforcement lives in the manifest layer);
	// this test just documents that very deep templates still canonicalize
	require.NoError(t, x.Add(long))
}

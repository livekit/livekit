// Copyright 2026 LiveKit, Inc.

package router

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zapcore"
)

const (
	mGET Mask = 1 << iota
	mPOST
	mPUT
)

type spec struct {
	tpl  string
	mask Mask
}

func get(tpls ...string) []spec {
	s := make([]spec, len(tpls))
	for i, t := range tpls {
		s[i] = spec{t, mGET}
	}
	return s
}

func build(t testing.TB, specs []spec) (*Router[string], *oracle) {
	t.Helper()
	b := NewBuilder[string]()
	o := &oracle{}
	for _, s := range specs {
		tpl, err := ParseTemplate(s.tpl)
		require.NoError(t, err, s.tpl)
		require.NoError(t, b.Add(tpl, s.mask, s.tpl))
		require.NoError(t, o.add(s.tpl, s.mask), s.tpl)
	}
	return b.Build(), o
}

// Every case asserts the expected answer and that the oracle agrees.
func TestMatchAdversarial(t *testing.T) {
	cases := []struct {
		name   string
		routes []spec
		path   string
		mask   Mask
		want   string
		res    Result
	}{
		// declaration order decides
		{"order beats specificity", get("/u/{id}", "/u/me"), "/u/me", mGET, "/u/{id}", ResultFull},
		{"order the other way", get("/u/me", "/u/{id}"), "/u/me", mGET, "/u/me", ResultFull},
		{"order with int first", get("/x/{a:int}", "/x/{b}"), "/x/42", mGET, "/x/{a:int}", ResultFull},
		{"order falls through to str", get("/x/{a:int}", "/x/{b}"), "/x/4x", mGET, "/x/{b}", ResultFull},
		{"literal buried deep", get("/a/{x}/c", "/a/b/c"), "/a/b/c", mGET, "/a/{x}/c", ResultFull},

		// the method dimension must not let a partial prune a later full match
		{"later method wins", []spec{{"/x", mGET}, {"/x", mPOST}}, "/x", mPOST, "/x", ResultFull},
		{"wildcard partial then literal full", []spec{{"/{a}", mGET}, {"/x", mPOST}}, "/x", mPOST, "/x", ResultFull},
		{"partial at a lower index", []spec{{"/a/{x}", mGET}, {"/a/b", mPOST}}, "/a/b", mPOST, "/a/b", ResultFull},
		{"no method matches", []spec{{"/x", mGET}, {"/x", mPOST}}, "/x", mPUT, "", ResultPartial},
		{"nothing matches", get("/x"), "/nope", mGET, "", ResultNone},

		// mid-segment params, which the walk must search for
		{"name dot ext", get("/f/{name}.{ext}"), "/f/a.b.c", mGET, "/f/{name}.{ext}", ResultFull},
		{"name dot ext needs a dot", get("/f/{name}.{ext}"), "/f/abc", mGET, "", ResultNone},
		{"suffix literal", get("/f/{name}.json"), "/f/a.b.json", mGET, "/f/{name}.json", ResultFull},
		{"prefix and suffix", get("/v{major}.{minor}"), "/v1.2", mGET, "/v{major}.{minor}", ResultFull},
		{"adjacent params", get("/{a}{b}"), "/xy", mGET, "/{a}{b}", ResultFull},

		// greedy path, which crosses '/'
		{"path backtracks to a literal", get("/{p:path}/end"), "/a/b/end/end", mGET, "/{p:path}/end", ResultFull},
		{"path matches empty", get("/files/{p:path}"), "/files/", mGET, "/files/{p:path}", ResultFull},
		{"path spans slashes", get("/files/{p:path}"), "/files/a/b/c.txt", mGET, "/files/{p:path}", ResultFull},
		{"path refuses newline", get("/files/{p:path}"), "/files/a\nb", mGET, "", ResultNone},
		{"str accepts newline", get("/files/{p}"), "/files/a\nb", mGET, "/files/{p}", ResultFull},

		// a template anchors as `\n?$`
		{"trailing newline accepted", get("/x"), "/x\n", mGET, "/x", ResultFull},
		{"two trailing newlines rejected", get("/x"), "/x\n\n", mGET, "", ResultNone},
		{"path convertor eats to the newline", get("/files/{p:path}"), "/files/a\n", mGET, "/files/{p:path}", ResultFull},

		// float's optional fraction backtracks
		{"float with fraction", get("/p/{v:float}.json"), "/p/1.25.json", mGET, "/p/{v:float}.json", ResultFull},
		{"float without fraction", get("/p/{v:float}.json"), "/p/1.json", mGET, "/p/{v:float}.json", ResultFull},
		{"float rejects a bare dot", get("/p/{v:float}"), "/p/1.", mGET, "", ResultNone},
		{"float takes the fraction", get("/p/{v:float}"), "/p/1.25", mGET, "/p/{v:float}", ResultFull},

		// int followed by a digit needs a shorter run
		{"int backtracks off a digit", get("/{n:int}0/x"), "/120/x", mGET, "/{n:int}0/x", ResultFull},

		// uuid admits exactly one length
		{"uuid hyphenated", get("/o/{u:uuid}"), "/o/123e4567-e89b-12d3-a456-426614174000", mGET, "/o/{u:uuid}", ResultFull},
		{"uuid bare", get("/o/{u:uuid}"), "/o/123e4567e89b12d3a456426614174000", mGET, "/o/{u:uuid}", ResultFull},
		{"uuid then literal", get("/o/{u:uuid}/z"), "/o/123e4567e89b12d3a456426614174000/z", mGET, "/o/{u:uuid}/z", ResultFull},
		{"uuid too short", get("/o/{u:uuid}"), "/o/123e4567", mGET, "", ResultNone},

		// radix splits
		{"split abc", get("/abc", "/abd", "/ab", "/a"), "/ab", mGET, "/ab", ResultFull},
		{"split abd", get("/abc", "/abd", "/ab", "/a"), "/abd", mGET, "/abd", ResultFull},
		{"split a", get("/abc", "/abd", "/ab", "/a"), "/a", mGET, "/a", ResultFull},
		{"split reversed", get("/a", "/ab", "/abd", "/abc"), "/abc", mGET, "/abc", ResultFull},
		{"split miss", get("/abc", "/abd"), "/abe", mGET, "", ResultNone},

		// literals are byte-exact
		{"case sensitive", get("/token"), "/Token", mGET, "", ResultNone},
		{"trailing slash is not implied", get("/token"), "/token/", mGET, "", ResultNone},
		{"registered with a slash", get("/token/"), "/token/", mGET, "/token/", ResultFull},

		// degenerate paths
		{"root", get("/"), "/", mGET, "/", ResultFull},
		{"double slash", get("/", "//"), "//", mGET, "//", ResultFull},
		{"empty segment", get("/a//b"), "/a//b", mGET, "/a//b", ResultFull},
		{"str will not cross a slash", get("/a/{x}"), "/a/b/c", mGET, "", ResultNone},
		{"str needs a byte", get("/a/{x}"), "/a/", mGET, "", ResultNone},
		{"a decoded %2F is just a slash", get("/files/{p}"), "/files/a/b", mGET, "", ResultNone},

		// a brace that opens nothing well-formed is a literal
		{"stray brace", get("/a{b"), "/a{b", mGET, "/a{b", ResultFull},
		{"empty convertor is literal", get("/x/{a:}"), "/x/{a:}", mGET, "/x/{a:}", ResultFull},
		{"digit-leading name is literal", get("/x/{1bad}"), "/x/{1bad}", mGET, "/x/{1bad}", ResultFull},
		{"doubled brace", get("/{{a}"), "/{x", mGET, "/{{a}", ResultFull},
	}

	for _, c := range cases {
		r, o := build(t, c.routes)

		got, res := r.Match(c.path, c.mask)
		require.Equal(t, c.res, res, "%s: %q", c.name, c.path)
		require.Equal(t, c.want, got, "%s: %q", c.name, c.path)

		oGot, oRes := o.match(c.path, c.mask)
		require.Equal(t, c.res, oRes, "oracle disagrees on %s: %q", c.name, c.path)
		require.Equal(t, c.want, oGot, "oracle disagrees on %s: %q", c.name, c.path)
	}
}

// Whether a template can force a search is fixed at build time.
func TestAmbiguousClassification(t *testing.T) {
	cases := []struct {
		tpl       string
		ambiguous bool
	}{
		{"/static", false},
		{"/u/{id}", false},
		{"/u/{id}/posts", false},
		{"/u/{id:int}/posts", false},
		{"/files/{p:path}", false},
		{"/o/{u:uuid}/z", false},
		{"/o/{u:uuid}{rest}", false},
		{"/f/{name}.json", true},
		{"/f/{name}.{ext}", true},
		{"/{p:path}/end", true},
		{"/p/{v:float}.json", true},
		{"/{n:int}0/x", true},
		{"/{a}{b}", true},
		{"/{n:int}/x", false},
		{"/{v:float}/x", false},
	}
	for _, c := range cases {
		r, _ := build(t, get(c.tpl))
		require.Equal(t, c.ambiguous, len(r.Ambiguous()) == 1, "%s", c.tpl)
	}
}

// An unambiguous table holds no edge that can search, so no path length reaches
// the budget; an ambiguous one gives up at it.
func TestStepBudget(t *testing.T) {
	r, _ := build(t, get("/api/{v:int}/res/{id}/sub/{name}"))
	for i := range r.nodes {
		for _, e := range r.nodes[i].kids {
			require.True(t, e.single, "%q", e.lit)
		}
	}
	_, res := r.Match("/api/1/res/"+strings.Repeat("x", 4096)+"/sub/y", mGET)
	require.Equal(t, ResultFull, res)

	r, _ = build(t, get("/{a}{b}{c}{d}x"))
	_, res = r.Match("/"+strings.Repeat("a", 4096), mGET)
	require.Equal(t, ResultOverBudget, res)
}

func TestEmptyRouter(t *testing.T) {
	r := NewBuilder[string]().Build()
	got, res := r.Match("/x", mGET)
	require.Equal(t, ResultNone, res)
	require.Empty(t, got)
}

// Readers match against a published table while writers swap in new ones.
func TestConcurrentSwapUnderLoad(t *testing.T) {
	tables := make([]*Router[string], 4)
	for i := range tables {
		tables[i], _ = build(t, get(
			fmt.Sprintf("/v%d/{id:int}", i),
			"/f/{name}.json",
			"/static/{p:path}",
			"/health",
		))
	}

	var h atomic.Pointer[Router[string]]
	h.Store(tables[0])

	var stop atomic.Bool
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for i := 0; !stop.Load(); i++ {
				h.Store(tables[i%len(tables)])
			}
		})
	}
	for range 8 {
		wg.Go(func() {
			for !stop.Load() {
				r := h.Load()
				r.Match("/v2/42", mGET)
				r.Match("/f/a.b.json", mGET)
				r.Match("/static/a/b/c", mGET)
				r.Match("/health", mPOST)
			}
		})
	}
	time.Sleep(200 * time.Millisecond)
	stop.Store(true)
	wg.Wait()
}

func TestMarshalLogObject(t *testing.T) {
	r, _ := build(t, get("/health", "/f/{name}.json"))

	enc := zapcore.NewMapObjectEncoder()
	require.NoError(t, r.MarshalLogObject(enc))
	require.Equal(t, 2, enc.Fields["routes"])
	require.Equal(t, []any{"/f/{name}.json"}, enc.Fields["ambiguous"])

	plain, _ := build(t, get("/health"))
	enc = zapcore.NewMapObjectEncoder()
	require.NoError(t, plain.MarshalLogObject(enc))
	require.NotContains(t, enc.Fields, "ambiguous")

	var nilRouter *Router[string]
	require.NoError(t, nilRouter.MarshalLogObject(zapcore.NewMapObjectEncoder()))
}

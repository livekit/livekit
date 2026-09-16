// Copyright 2026 LiveKit, Inc.

package endpoint

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/agent/endpoint/router"
)

func mustManifest(t *testing.T, eps ...*livekit.AgentHttp_AgentEndpoint) *Manifest {
	t.Helper()
	m, err := ParseManifest(eps)
	require.NoError(t, err)
	return m
}

func regOf(workerID string, m *Manifest) *Registration {
	return NewRegistration(RegistrationParams{
		WorkerID: workerID, Manifest: m, Session: &fakeSession{},
	})
}

func testScope() *Scope { return NewScope(logger.GetLogger()) }

// tableOf registers one worker per manifest into a single scope and returns the
// deployment's merged table.
func tableOf(t *testing.T, manifests ...*Manifest) *routeTable {
	t.Helper()
	g, s := NewRegistry(), testScope()
	for i, m := range manifests {
		g.Register(s, regOf(fmt.Sprintf("w%d", i), m))
	}
	tbl := s.routeTable()
	require.NotNil(t, tbl)
	return tbl
}

// serving names the workers a request would be dispatched to, sorted.
func serving(tbl *routeTable, path, method string, granted bool) []string {
	matched, _, _, _ := tbl.match(path, methodMask(method), granted)
	ids := make([]string, 0, len(matched))
	for _, w := range matched {
		ids = append(ids, w.reg.WorkerID)
	}
	slices.Sort(ids)
	return ids
}

// A mixed fleet serves the union of its routes, and a route only ever
// dispatches to the workers that declared it.
func TestRouteTableUnion(t *testing.T) {
	tbl := tableOf(t,
		mustManifest(t, ep("/health", []string{"GET"}, true), ep("/v1", []string{"GET"}, true)),
		mustManifest(t, ep("/health", []string{"GET"}, true), ep("/v2", []string{"GET"}, true)),
	)

	require.Equal(t, []string{"w0", "w1"}, serving(tbl, "/health", http.MethodGet, true))
	require.Equal(t, []string{"w0"}, serving(tbl, "/v1", http.MethodGet, true))
	require.Equal(t, []string{"w1"}, serving(tbl, "/v2", http.MethodGet, true))
	require.Empty(t, serving(tbl, "/v3", http.MethodGet, true))
}

// A worker leaving takes only the routes nothing else declares.
func TestRouteTableDeparture(t *testing.T) {
	g, s := NewRegistry(), testScope()
	old := regOf("w0", mustManifest(t, ep("/health", []string{"GET"}, true), ep("/v1", []string{"GET"}, true)))
	g.Register(s, old)
	g.Register(s, regOf("w1", mustManifest(t, ep("/health", []string{"GET"}, true), ep("/v2", []string{"GET"}, true))))
	tbl := s.routeTable()

	g.Deregister(old)

	require.Equal(t, []string{"w1"}, serving(tbl, "/health", http.MethodGet, true))
	require.Empty(t, serving(tbl, "/v1", http.MethodGet, true), "the departing worker's own route is gone")
	require.Equal(t, []string{"w1"}, serving(tbl, "/v2", http.MethodGet, true))

	// the last worker out drops the table with it
	g.Deregister(s.Candidates()[0])
	require.Nil(t, s.routeTable(), "the last worker out drops the table, so the front reports 503 not 404")
	require.True(t, s.Empty())
}

// Declaration order is the whole priority rule, and a homogeneous fleet keeps
// the order its manifest declared: a literal registered ahead of a param that
// would also match it must still win.
func TestRouteTableMergeOrder(t *testing.T) {
	m := mustManifest(t, ep("/users/me", []string{"GET"}, true), ep("/users/{id}", []string{"GET"}, true))
	tbl := tableOf(t, m, m)

	_, r, res, _ := tbl.match("/users/me", methodMask(http.MethodGet), true)
	require.Equal(t, router.ResultFull, res)
	require.Equal(t, "/users/me", r.Template.String())
	require.Equal(t, []string{"w0", "w1"}, serving(tbl, "/users/me", http.MethodGet, true))

	_, r, _, _ = tbl.match("/users/7", methodMask(http.MethodGet), true)
	require.Equal(t, "/users/{id}", r.Template.String())
}

// Workers may disagree about a route's visibility mid-rollout. An ungranted
// caller is narrowed to the workers that declared it public.
func TestRouteTablePerWorkerPublic(t *testing.T) {
	tbl := tableOf(t,
		mustManifest(t, ep("/x", []string{"GET"}, true)),
		mustManifest(t, ep("/x", []string{"GET"}, false)),
	)

	require.Equal(t, []string{"w0", "w1"}, serving(tbl, "/x", http.MethodGet, true))
	require.Equal(t, []string{"w0"}, serving(tbl, "/x", http.MethodGet, false))

	matched, _, _, denied := tbl.match("/x", methodMask(http.MethodGet), false)
	require.Len(t, matched, 1)
	require.True(t, denied, "the private declaration is reported, not silently dropped")

	// no public declarer at all denies outright
	private := tableOf(t, mustManifest(t, ep("/y", []string{"GET"}, false)))
	matched, _, _, denied = private.match("/y", methodMask(http.MethodGet), false)
	require.Empty(t, matched)
	require.True(t, denied)
}

// One route per method: workers declaring different verbs on one path each
// serve their own, and a verb nobody declared is partial.
func TestRouteTableMethodSplit(t *testing.T) {
	tbl := tableOf(t,
		mustManifest(t, ep("/thing", []string{"GET"}, true)),
		mustManifest(t, ep("/thing", []string{"POST", "DELETE"}, true)),
	)

	require.Equal(t, []string{"w0"}, serving(tbl, "/thing", http.MethodGet, true))
	require.Equal(t, []string{"w1"}, serving(tbl, "/thing", http.MethodPost, true))
	require.Equal(t, []string{"w1"}, serving(tbl, "/thing", http.MethodDelete, true))

	_, _, res, _ := tbl.match("/thing", methodMask(http.MethodPut), true)
	require.Equal(t, router.ResultPartial, res)
}

// Param names are invisible to the matcher, so two spellings of one shape are
// one route serving both workers, each carrying its own spelling.
func TestRouteTableCanonicalMerge(t *testing.T) {
	tbl := tableOf(t,
		mustManifest(t, ep("/items/{id}", []string{"GET"}, true)),
		mustManifest(t, ep("/items/{key}", []string{"GET"}, true)),
	)

	require.Len(t, tbl.routes, 1, "one shape is one route")
	matched, _, res, _ := tbl.match("/items/7", methodMask(http.MethodGet), true)
	require.Equal(t, router.ResultFull, res)
	require.Len(t, matched, 2)

	spelling := map[string]string{}
	for _, w := range matched {
		spelling[w.reg.WorkerID] = w.raw
	}
	require.Equal(t, map[string]string{"w0": "/items/{id}", "w1": "/items/{key}"}, spelling)

	// a different shape stays a different route
	require.Len(t, tableOf(t,
		mustManifest(t, ep("/items/{id}", []string{"GET"}, true)),
		mustManifest(t, ep("/items/{id:int}", []string{"GET"}, true)),
	).routes, 2)
}

// A literal brace is not a param, and must not collide with one.
func TestRouteTableLiteralBraceDistinctFromParam(t *testing.T) {
	tbl := tableOf(t,
		mustManifest(t, ep("/x/{a}", []string{"GET"}, true)),
		mustManifest(t, ep("/x/{:str}", []string{"GET"}, true)),
	)
	require.Len(t, tbl.routes, 2)
}

// A manifest may name one path and method twice; the worker joins the route
// once.
func TestRouteTableDuplicateDeclaration(t *testing.T) {
	tbl := tableOf(t, mustManifest(t,
		ep("/x", []string{"GET", "POST"}, true),
		ep("/x", []string{"GET"}, true),
	))
	require.Equal(t, []string{"w0"}, serving(tbl, "/x", http.MethodGet, true))
}

// The published tree holds routes by pointer, so a reconnect that rebuilds the
// same route set keeps those pointers and stays routable.
func TestRouteTableSupersedeStaysRoutable(t *testing.T) {
	g, s := NewRegistry(), testScope()
	m := mustManifest(t, ep("/x", []string{"GET"}, true))
	g.Register(s, regOf("w0", m))
	tbl := s.routeTable()
	before := tbl.tree.Load()

	g.Register(s, regOf("w0", m))

	require.Equal(t, []string{"w0"}, serving(tbl, "/x", http.MethodGet, true),
		"a reconnect with an unchanged manifest stays routable")
	require.Same(t, before, tbl.tree.Load(), "an unchanged route set needs no rebuild")
}

// A reconnect may land on a different agent name or deployment, i.e. a different
// scope. The retiring epoch's routes live in the old scope and must be retracted
// there.
func TestRouteTableSupersedeAcrossScopes(t *testing.T) {
	g := NewRegistry()
	from, to := testScope(), testScope()
	m := mustManifest(t, ep("/x", []string{"GET"}, true))
	firstSess := &fakeSession{}
	first := NewRegistration(RegistrationParams{WorkerID: "w0", Manifest: m, Session: firstSess})
	g.Register(from, first)

	g.Register(to, regOf("w0", m))

	require.Nil(t, from.routeTable(), "the old scope's table is gone")
	require.True(t, from.Empty())
	require.Equal(t, []string{"w0"}, serving(to.routeTable(), "/x", http.MethodGet, true))
	require.True(t, firstSess.closed, "the superseded epoch's session is closed")

	// the retiring control connection tears down afterwards and must not strand
	// the new epoch
	g.Deregister(first)
	require.Equal(t, []string{"w0"}, serving(to.routeTable(), "/x", http.MethodGet, true))
}

// The tree is rebuilt exactly when the merged route set or its order moves.
func TestRouteTableRebuildTrigger(t *testing.T) {
	g, s := NewRegistry(), testScope()
	m := mustManifest(t, ep("/x", []string{"GET"}, true))
	g.Register(s, regOf("w0", m))
	tbl := s.routeTable()

	before := tbl.tree.Load()
	g.Register(s, regOf("w1", m))
	require.Same(t, before, tbl.tree.Load(), "a worker joining an existing route changes no route")

	g.Register(s, regOf("w2", mustManifest(t, ep("/x", []string{"GET"}, true), ep("/y", []string{"GET"}, true))))
	require.NotSame(t, before, tbl.tree.Load(), "a new route rebuilds")
}

// A departure can raise a route's merge position and an arrival lower it;
// either reorders the table.
func TestRouteTableOrderFollowsDeparture(t *testing.T) {
	g, s := NewRegistry(), testScope()
	first := regOf("w0", mustManifest(t,
		ep("/a/{x}", []string{"GET"}, true),
		ep("/a/b", []string{"GET"}, true),
	))
	g.Register(s, first)
	g.Register(s, regOf("w1", mustManifest(t,
		ep("/a/b", []string{"GET"}, true),
		ep("/a/{x}", []string{"GET"}, true),
	)))
	tbl := s.routeTable()

	// both routes hold position 0 - w0 declares the param first, w1 the literal
	// - so the tie-break decides, and the param shadows the literal
	_, r, _, _ := tbl.match("/a/b", methodMask(http.MethodGet), true)
	require.Equal(t, "/a/{x}", r.Template.String())

	// w0 leaving raises the param to w1's position 1, putting the literal ahead
	// of it
	g.Deregister(first)
	_, r, _, _ = tbl.match("/a/b", methodMask(http.MethodGet), true)
	require.Equal(t, "/a/b", r.Template.String())
}

// Ambiguity is a property of the merged tree: one worker's shape can carry the
// whole deployment's matcher over its step budget, and an undecided route has no
// Public flag left to clear an ungranted request.
func TestRouteTableAmbiguityIsContagious(t *testing.T) {
	long := "/" + strings.Repeat("a", 512)

	open := tableOf(t, mustManifest(t, ep("/{p:path}", []string{"GET"}, true)))
	_, _, res, _ := open.match(long, methodMask(http.MethodGet), false)
	require.Equal(t, router.ResultFull, res, "alone, the catch-all decides")

	merged := tableOf(t,
		mustManifest(t, ep("/{a}{b}{c}{d}x", []string{"GET"}, true)),
		mustManifest(t, ep("/{p:path}", []string{"GET"}, true)),
	)
	_, _, res, _ = merged.match(long, methodMask(http.MethodGet), false)
	require.Equal(t, router.ResultOverBudget, res)
}

// KNOWN LIMITATION: the merge takes each route's lowest position across the
// fleet, which does not preserve any one worker's internal order. A worker can
// be handed a request the merged table resolved to a public route while its own
// table routes that path to a private one. An order-preserving merge of the
// declaration chains would hold the property.
func TestRouteTableMergeOrderInversion(t *testing.T) {
	tbl := tableOf(t,
		mustManifest(t,
			ep("/x/{z:int}", []string{"GET"}, false),
			ep("/x/{b}", []string{"GET"}, true),
		),
		mustManifest(t, ep("/x/{b}", []string{"GET"}, true)),
	)

	_, r, res, _ := tbl.match("/x/7", methodMask(http.MethodGet), false)
	require.Equal(t, router.ResultFull, res)
	require.Equal(t, "/x/{b}", r.Template.String())
	require.Equal(t, []string{"w0", "w1"}, serving(tbl, "/x/7", http.MethodGet, false),
		"w0 is reachable anonymously while its own table routes /x/7 to a private route")
}

// Registrations churn while requests resolve; run under -race.
func TestRouteTableConcurrentChurn(t *testing.T) {
	g, sc := NewRegistry(), testScope()
	stable := regOf("stable", mustManifest(t, ep("/x", []string{"GET"}, true)))
	g.Register(sc, stable)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m := mustManifest(t, ep("/x", []string{"GET"}, true), ep(fmt.Sprintf("/w%d", i), []string{"GET"}, true))
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				r := regOf(fmt.Sprintf("w%d", i), m)
				g.Register(sc, r)
				g.Deregister(r)
			}
		}()
	}
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if tbl := sc.routeTable(); tbl != nil {
					tbl.match("/x", methodMask(http.MethodGet), true)
					tbl.serves("/x/", methodMask(http.MethodGet))
				}
			}
		}()
	}
	for range 2000 {
		if tbl := sc.routeTable(); tbl != nil {
			tbl.match("/x", methodMask(http.MethodGet), false)
		}
	}
	close(stop)
	wg.Wait()

	require.Contains(t, serving(sc.routeTable(), "/x", http.MethodGet, true), "stable")
}

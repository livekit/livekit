// Copyright 2026 LiveKit, Inc.

//go:build !race

package router

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The race runtime allocates, so this cannot run under -race.
func TestMatchZeroAlloc(t *testing.T) {
	specs := benchSpecs(256)
	r, _ := build(t, specs)
	for _, c := range benchCases(specs) {
		n := testing.AllocsPerRun(200, func() { r.Match(c.path, c.mask) })
		require.Zero(t, n, c.name)
	}

	// an ambiguous table searches, and must not allocate while it does
	amb, _ := build(t, get("/f/{name}.{ext}", "/{p:path}/end"))
	n := testing.AllocsPerRun(200, func() { amb.Match("/f/a.b.c", mGET) })
	require.Zero(t, n, "ambiguous")
}

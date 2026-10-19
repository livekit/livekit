// Copyright 2026 LiveKit, Inc.

package router

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// fragments compose into templates. %d takes a per-template counter so
// generated param names never collide, which ParseTemplate rejects.
var fragments = []string{
	"/a", "/b", "/ab", "/abc", "/",
	"/{p%d}", "/{n%d:int}", "/{f%d:float}", "/{u%d:uuid}", "/{r%d:path}",
	"{q%d}", ".json", ".{e%d}", "-{s%d}", "0", "/end", "//", "\n", "{", "x",
}

func genSpecs(script []byte) []spec {
	var specs []spec
	var b strings.Builder
	n := 0
	flush := func() {
		if b.Len() > 0 {
			specs = append(specs, spec{"/" + b.String(), Mask(n%7) + 1})
			b.Reset()
		}
	}
	for _, c := range script {
		if int(c)%(len(fragments)+2) >= len(fragments) {
			flush()
			continue
		}
		fmt.Fprintf(&b, fragments[int(c)%len(fragments)], n)
		n++
	}
	flush()
	if len(specs) > 64 {
		specs = specs[:64]
	}
	return specs
}

// The trie answers what a linear scan of compiled starlette patterns answers:
// the same winning template and the same result, for every table and path.
func FuzzMatchAgainstOracle(f *testing.F) {
	f.Add([]byte{5, 200, 0, 5}, "/x/y", uint8(1))
	f.Add([]byte{5, 1, 200, 1, 5}, "/a/b", uint8(1))
	f.Add([]byte{9, 200, 15}, "/a/b/end", uint8(1))
	f.Add([]byte{5, 12, 200, 5}, "/f/a.b.json", uint8(1))
	f.Add([]byte{7, 11, 200, 7}, "/1.25.json", uint8(3))
	f.Add([]byte{8, 200, 8, 15}, "/123e4567e89b12d3a456426614174000", uint8(1))
	f.Add([]byte{0, 1, 2, 3, 200, 0, 200, 2}, "/ab", uint8(1))
	f.Add([]byte{5, 200, 5, 200, 5}, "/x\n", uint8(7))
	f.Add([]byte{13, 5, 200, 5}, "/a-b", uint8(1))

	f.Fuzz(func(t *testing.T, script []byte, path string, q uint8) {
		specs := genSpecs(script)
		if len(specs) == 0 {
			return
		}

		b := NewBuilder[string]()
		o := &oracle{}
		added := 0
		for _, s := range specs {
			tpl, err := ParseTemplate(s.tpl)
			if err != nil {
				continue
			}
			require.NoError(t, o.add(s.tpl, s.mask), "oracle rejected an accepted template %q", s.tpl)
			require.NoError(t, b.Add(tpl, s.mask, s.tpl))
			added++
		}
		if added == 0 {
			return
		}
		r := b.Build()

		mask := Mask(q)
		got, res := r.Match(path, mask)
		if res == ResultOverBudget {
			return
		}
		wantTpl, wantRes := o.match(path, mask)
		require.Equal(t, wantRes, res, "path %q mask %d over %v", path, mask, specs)
		require.Equal(t, wantTpl, got, "path %q mask %d over %v", path, mask, specs)
	})
}

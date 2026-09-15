// Copyright 2026 LiveKit, Inc.

package router

import (
	"fmt"
	"testing"
)

// benchSpecs is shaped like a FastAPI app: a few static routes, then resource
// collections with an id param, then a catch-all.
func benchSpecs(n int) []spec {
	specs := []spec{
		{"/health", mGET},
		{"/ready", mGET},
		{"/metrics", mGET},
	}
	for i := len(specs); i < n-1; i += 2 {
		res := fmt.Sprintf("/api/v1/res%d", i)
		specs = append(specs, spec{res, mGET | mPOST})
		specs = append(specs, spec{res + "/{id:int}", mGET | mPUT})
	}
	specs = append(specs, spec{"/static/{p:path}", mGET})
	return specs[:min(n, len(specs))]
}

type benchCase struct {
	name, path string
	mask       Mask
}

func benchCases(specs []spec) []benchCase {
	last := specs[len(specs)-2].tpl
	return []benchCase{
		{"hit_first", "/health", mGET},
		{"hit_last", last[:len(last)-len("/{id:int}")] + "/42", mGET},
		{"method_not_allowed", "/health", mPOST},
		{"miss", "/api/v1/nope/42", mGET},
	}
}

func BenchmarkMatch(b *testing.B) {
	for _, n := range []int{8, 64, 256} {
		specs := benchSpecs(n)
		r, _ := build(b, specs)
		for _, c := range benchCases(specs) {
			b.Run(fmt.Sprintf("routes=%d/%s", n, c.name), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					r.Match(c.path, c.mask)
				}
			})
		}
	}
}

// No shared prefixes, so the root's edge scan cannot be narrowed.
func BenchmarkMatchWideFanout(b *testing.B) {
	specs := make([]spec, 0, 256)
	for i := range 256 {
		specs = append(specs, spec{fmt.Sprintf("/%c%c/x", 'a'+i/16, 'a'+i%16), mGET})
	}
	r, _ := build(b, specs)
	b.ReportAllocs()
	for b.Loop() {
		r.Match("/pp/x", mGET)
	}
}

func BenchmarkBuild(b *testing.B) {
	specs := benchSpecs(256)
	tpls := make([]*Template, len(specs))
	for i, s := range specs {
		t, err := ParseTemplate(s.tpl)
		if err != nil {
			b.Fatal(err)
		}
		tpls[i] = t
	}
	b.ReportAllocs()
	for b.Loop() {
		bld := NewBuilder[string]()
		for i, t := range tpls {
			_ = bld.Add(t, specs[i].mask, specs[i].tpl)
		}
		bld.Build()
	}
}

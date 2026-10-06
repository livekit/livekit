// Copyright 2026 LiveKit, Inc.

package router

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func matches(t *testing.T, tpl, path string) bool {
	t.Helper()
	r, o := build(t, get(tpl))
	got, res := r.Match(path, mGET)
	oGot, oRes := o.match(path, mGET)
	require.Equal(t, oRes, res, "oracle disagrees: %q vs %q", tpl, path)
	require.Equal(t, oGot, got, "oracle disagrees: %q vs %q", tpl, path)
	return res == ResultFull
}

func TestTemplateStarletteSemantics(t *testing.T) {
	cases := []struct {
		template string
		path     string
		match    bool
	}{
		{"/token", "/token", true},
		{"/token", "/token/", false},
		{"/token", "/Token", false},
		{"/users/{id}", "/users/42", true},
		{"/users/{id}", "/users/42/posts", false},
		{"/users/{id}", "/users/", false},
		{"/users/{id:int}", "/users/42", true},
		{"/users/{id:int}", "/users/4x2", false},
		{"/files/{p:path}", "/files/a/b/c.txt", true},
		{"/files/{p:path}", "/files/", true},
		{"/price/{v:float}", "/price/1.25", true},
		{"/price/{v:float}", "/price/1.", false},
		{"/obj/{u:uuid}", "/obj/123e4567-e89b-12d3-a456-426614174000", true},
		// starlette's uuid convertor makes every hyphen optional
		{"/obj/{u:uuid}", "/obj/123e4567e89b12d3a456426614174000", true},
		{"/obj/{u:uuid}", "/obj/123e4567", false},
		{"/a/{x}/b/{y}", "/a/1/b/2", true},
		{"/a/{x}/b/{y}", "/a/1/c/2", false},
	}
	for _, c := range cases {
		require.Equal(t, c.match, matches(t, c.template, c.path), "%s vs %s", c.template, c.path)
	}
}

// Vectors generated from CPython against a transcription of compile_path.
func TestTemplateTrailingNewline(t *testing.T) {
	cases := []struct {
		template string
		path     string
		match    bool
	}{
		{"/x", "/x\n", true},
		{"/x", "/x\n\n", false},
		{"/x", "/x\nz", false},
		{"/files/{p:path}", "/files/a\n", true},
		{"/files/{p:path}", "/files/a\nb", false},
		{"/files/{p}", "/files/a\n", true},
		{"/files/{p}", "/files/a\nb", true},
		{"/n/{i:int}", "/n/42\n", true},
		{"/n/{i:int}", "/n/4\n2", false},
		{"/", "/\n", true},
		{"/a/", "/a/\n", true},
	}
	for _, c := range cases {
		require.Equal(t, c.match, matches(t, c.template, c.path), "%q vs %q", c.template, c.path)
	}
}

func TestParseTemplateRejects(t *testing.T) {
	for _, tpl := range []string{
		"/x/{id:slug}",
		"/x/{a}/{a}",
		"no-slash",
		"/x/\xff",
	} {
		_, err := ParseTemplate(tpl)
		require.Error(t, err, tpl)
	}
}

// A brace that opens nothing well-formed is an ordinary literal.
func TestParseTemplateLenientBraces(t *testing.T) {
	for _, tpl := range []string{"/a{b", "/x/{a:}", "/x/{1bad}", "/{{a}", "/}", "/{}"} {
		tt, err := ParseTemplate(tpl)
		require.NoError(t, err, tpl)
		require.Equal(t, tpl, tt.String())
	}
}

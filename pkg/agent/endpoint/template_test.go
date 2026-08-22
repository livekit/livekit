package endpoint

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
)

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
		tpl, err := ParseTemplate(c.template)
		require.NoError(t, err, c.template)
		require.Equal(t, c.match, tpl.Match(c.path), "%s vs %s", c.template, c.path)
	}
}

func TestTemplateRejectsCustomConvertors(t *testing.T) {
	_, err := ParseTemplate("/x/{id:slug}")
	require.Error(t, err)
	_, err = ParseTemplate("/x/{a}/{a}")
	require.Error(t, err)
	_, err = ParseTemplate("no-slash")
	require.Error(t, err)
}

func ep(path string, methods []string, public bool) *livekit.AgentHttp_AgentEndpoint {
	return &livekit.AgentHttp_AgentEndpoint{Path: path, Methods: methods, Public: public}
}

func TestManifestFullPartialSemantics(t *testing.T) {
	// POST /x registered after GET /x must still serve POSTs (starlette scans
	// for a FULL match before settling for the PARTIAL 405)
	m, err := ParseManifest([]*livekit.AgentHttp_AgentEndpoint{
		ep("/x", []string{"GET"}, true),
		ep("/x", []string{"POST"}, true),
	})
	require.NoError(t, err)

	r, res := m.Match("/x", http.MethodPost)
	require.Equal(t, MatchFull, res)
	require.Contains(t, r.Methods, "POST")

	// the manifest carries the app's methods verbatim: FastAPI does not imply
	// HEAD from GET, so neither does the matcher
	_, res = m.Match("/x", http.MethodHead)
	require.Equal(t, MatchPartial, res)

	// PARTIAL only when no route serves the method
	_, res = m.Match("/x", http.MethodDelete)
	require.Equal(t, MatchPartial, res)

	_, res = m.Match("/nope", http.MethodGet)
	require.Equal(t, MatchNone, res)
}

func TestManifestRedirectSlashes(t *testing.T) {
	m, err := ParseManifest([]*livekit.AgentHttp_AgentEndpoint{
		ep("/hook", []string{"POST"}, true),
	})
	require.NoError(t, err)

	alt, ok := m.RedirectSlashes("/hook/", http.MethodPost)
	require.True(t, ok)
	require.Equal(t, "/hook", alt)

	_, ok = m.RedirectSlashes("/other/", http.MethodPost)
	require.False(t, ok)
}

func TestManifestValidation(t *testing.T) {
	_, err := ParseManifest([]*livekit.AgentHttp_AgentEndpoint{ep("/x", nil, false)})
	require.Error(t, err, "http endpoint without methods")

	_, err = ParseManifest([]*livekit.AgentHttp_AgentEndpoint{ep("/x", []string{"get"}, false)})
	require.Error(t, err, "lowercase method")

	_, err = ParseManifest([]*livekit.AgentHttp_AgentEndpoint{
		{Path: "/ws", Kind: livekit.AgentHttp_AEK_TEXT, Methods: []string{"GET"}},
	})
	require.Error(t, err, "unsupported endpoint kind")
}

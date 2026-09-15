// Copyright 2026 LiveKit, Inc.

package endpoint

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"

	"github.com/livekit/livekit-server/pkg/agent/endpoint/router"
)

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

	r, res := m.Match("/x", methodMask(http.MethodPost))
	require.Equal(t, router.ResultFull, res)
	require.Equal(t, "/x", r.Template.String())

	// the manifest carries the app's methods verbatim: FastAPI does not imply
	// HEAD from GET, so neither does the matcher
	_, res = m.Match("/x", methodMask(http.MethodHead))
	require.Equal(t, router.ResultPartial, res)

	// PARTIAL only when no route serves the method
	_, res = m.Match("/x", methodMask(http.MethodDelete))
	require.Equal(t, router.ResultPartial, res)

	// an unroutable method masks to 0
	_, res = m.Match("/x", methodMask("BREW"))
	require.Equal(t, router.ResultPartial, res)

	_, res = m.Match("/nope", methodMask(http.MethodGet))
	require.Equal(t, router.ResultNone, res)
}

func TestSlashAlternatePaths(t *testing.T) {
	m, err := ParseManifest([]*livekit.AgentHttp_AgentEndpoint{
		ep("/hook", []string{"POST"}, true),
		ep("/", []string{"POST"}, true),
	})
	require.NoError(t, err)
	candidates := []*Registration{{Manifest: m}}

	cases := []struct {
		path, escPath string
		alt, altEsc   string
		ok            bool
	}{
		// a slash-mismatched request normalizes to the registered form
		{"/hook/", "/hook/", "/hook", "/hook", true},
		// no route matches either slash form
		{"/other/", "/other/", "/other/", "/other/", false},
		// %2F decodes to a slash without being a separator
		{"/hook/", "/hook%2F", "/hook/", "/hook%2F", false},
		// "//" trims to "/" in both forms
		{"//", "//", "/", "/", true},
		// "/" has no alternate to try
		{"/", "/", "/", "/", false},
	}
	for _, c := range cases {
		alt, altEsc, ok := slashAlternatePaths(candidates, c.path, c.escPath, methodMask(http.MethodPost))
		require.Equal(t, c.ok, ok, "%q %q", c.path, c.escPath)
		require.Equal(t, c.alt, alt, "%q %q", c.path, c.escPath)
		require.Equal(t, c.altEsc, altEsc, "%q %q", c.path, c.escPath)
	}
}

// Templates anchor as `\n?$`: path refuses '\n' where str accepts it, so a
// decoded %0A would otherwise change which route wins.
func TestManifestTrailingNewlineRouteIdentity(t *testing.T) {
	m, err := ParseManifest([]*livekit.AgentHttp_AgentEndpoint{
		ep("/files/{p:path}", []string{"GET"}, false),
		ep("/files/{p}", []string{"GET"}, true),
	})
	require.NoError(t, err)

	r, res := m.Match("/files/x\n", methodMask(http.MethodGet))
	require.Equal(t, router.ResultFull, res)
	require.Equal(t, "/files/{p:path}", r.Template.String())
	require.False(t, r.Public, "the private route must win, as it does on the worker")
}

func TestManifestValidation(t *testing.T) {
	_, err := ParseManifest([]*livekit.AgentHttp_AgentEndpoint{ep("/x", nil, false)})
	require.Error(t, err, "http endpoint without methods")

	_, err = ParseManifest([]*livekit.AgentHttp_AgentEndpoint{ep("/x", []string{"get"}, false)})
	require.Error(t, err, "lowercase method")

	_, err = ParseManifest([]*livekit.AgentHttp_AgentEndpoint{ep("/x", []string{"GTE"}, false)})
	require.Error(t, err, "unsupported method rejected")

	_, err = ParseManifest([]*livekit.AgentHttp_AgentEndpoint{ep("no-slash", []string{"GET"}, false)})
	require.Error(t, err, "template without a leading slash")

	_, err = ParseManifest([]*livekit.AgentHttp_AgentEndpoint{ep("/x/{a:slug}", []string{"GET"}, false)})
	require.Error(t, err, "custom convertor")

	// the full FastAPI/starlette verb set is accepted
	_, err = ParseManifest([]*livekit.AgentHttp_AgentEndpoint{
		ep("/x", []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "TRACE"}, false),
	})
	require.NoError(t, err, "all FastAPI verbs accepted")

	_, err = ParseManifest([]*livekit.AgentHttp_AgentEndpoint{
		{Path: "/ws", Kind: livekit.AgentHttp_AEK_TEXT, Methods: []string{"GET"}},
	})
	require.Error(t, err, "unsupported endpoint kind")
}

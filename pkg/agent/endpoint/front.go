// Copyright 2026 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package endpoint

import (
	"bufio"
	"context"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/livekit/protocol/logger"
)

// HeaderEndpointMiss marks a response the front produced ITSELF - a routing
// miss (no matching route, wrong method, auth required, or no local capacity) -
// as distinct from a response the worker's app returned through the bridge. A
// relay caller keys its cross-node retry on this header, so a worker's own 404
// (e.g. GET /users/999 for a missing user) is never mistaken for "this node
// can't serve the path" and re-relayed. The value is the miss kind, so the
// caller can surface the most informative aggregate status. The header is set
// only on the private relay listener (see MarkMisses) and stripped by the relay
// caller, so it never reaches a client.
const HeaderEndpointMiss = "X-Livekit-Endpoint-Miss"

const (
	MissNotFound         = "notfound"
	MissMethodNotAllowed = "methodnotallowed"
	MissUnauthenticated  = "unauthenticated"
	MissUnavailable      = "unavailable"
)

const (
	// PathPrefix is the public route namespace: /agents/{agent_name}/{deployment}/{path...}
	PathPrefix = "/agents/"

	// responseHeadTimeout bounds the wait for the worker's response head. Bodies
	// (SSE, long streams) are unbounded; the head never legitimately takes this
	// long.
	responseHeadTimeout = 90 * time.Second

	// maxAttempts bounds worker retries per request
	maxAttempts = 3
)

// Per-request scratch is pooled: a served request would otherwise allocate a
// 32KiB response-copy buffer and a response-head reader every time, and at high
// request rates that dominates the front's garbage.
var (
	copyBufferPool   = sync.Pool{New: func() any { b := make([]byte, 32<<10); return &b }}
	responseReadPool = sync.Pool{New: func() any { return bufio.NewReaderSize(nil, 4<<10) }}
)

// APIKeyResolver maps an inbound request to the api key it is authorized for
// (empty when unauthenticated) - the service layer implements it from validated
// grants.
type APIKeyResolver func(r *http.Request) (apiKey string, authenticated bool)

type Front struct {
	registry      *Registry
	resolveAPIKey APIKeyResolver
	logger        logger.Logger

	// fallback is consulted when nothing local can serve the request (no
	// candidates, no route match, or every match without capacity); a
	// multi-node deployment plugs its resolve-and-relay here. nil means local
	// misses are final.
	fallback Fallback
	// see WithSingleKeyFallback
	singleKeyFallback bool
	// see MarkMisses: set on the private relay listener so a relay caller can
	// tell a routing miss from a worker-app response
	markMisses bool
}

func NewFront(registry *Registry, resolveAPIKey APIKeyResolver, log logger.Logger) *Front {
	return &Front{
		registry:      registry,
		resolveAPIKey: resolveAPIKey,
		logger:        log.WithComponent("agents.endpoint"),
	}
}

// FallbackRequest describes a request nothing local could serve. The request
// body is untouched when the fallback runs.
type FallbackRequest struct {
	// APIKey is the identity the front resolved the request to (empty when
	// unauthenticated)
	APIKey        string
	Authenticated bool
	AgentName     string
	Deployment    string
	// Method is the request's HTTP method, so a multi-node layer can select
	// candidates method-aware (a node serving the path under a different method
	// is not a candidate).
	Method string
	// Path within the deployment, '/'-rooted
	Path string
}

// Fallback serves a request elsewhere (e.g. a multi-node relay); it reports
// whether a response was written. Returning false falls back to the local
// status mapping.
type Fallback func(w http.ResponseWriter, r *http.Request, req *FallbackRequest) bool

// WithFallback installs the miss handler consulted when nothing local can
// serve a request.
func (f *Front) WithFallback(fb Fallback) *Front {
	f.fallback = fb
	return f
}

// MarkMisses tags the front's own routing-miss responses with HeaderEndpointMiss
// so a relay caller can distinguish them from worker-app responses. Set it on
// the private relay listener only; the public front must not (the header would
// leak to clients, and its misses are final anyway).
func (f *Front) MarkMisses() *Front {
	f.markMisses = true
	return f
}

// writeMiss writes a front-originated miss, tagging it with the kind when this
// front marks misses (the relay listener) so the relay caller can retry past it
// and aggregate the most informative status.
func (f *Front) writeMiss(w http.ResponseWriter, status int, kind, msg string) {
	if f.markMisses {
		w.Header().Set(HeaderEndpointMiss, kind)
	}
	http.Error(w, msg, status)
}

// writeUnavailable writes a 503 with a Retry-After hint, the miss the front
// returns when no worker can currently serve a request it did route.
func (f *Front) writeUnavailable(w http.ResponseWriter, msg string) {
	w.Header().Set("Retry-After", "1")
	f.writeMiss(w, http.StatusServiceUnavailable, MissUnavailable, msg)
}

// WithSingleKeyFallback resolves unauthenticated requests to the registry's
// single api key when the resolver yields none. Self-hosted convenience only: a
// multi-tenant front must never guess an api key from what happens to be
// registered.
func (f *Front) WithSingleKeyFallback() *Front {
	f.singleKeyFallback = true
	return f
}

func (f *Front) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest, ok := strings.CutPrefix(r.URL.Path, PathPrefix)
	if !ok {
		http.NotFound(w, r)
		return
	}
	agentName, rest, found := strings.Cut(rest, "/")
	if !found || agentName == "" {
		http.NotFound(w, r)
		return
	}
	deployment, path, found := strings.Cut(rest, "/")
	if !found {
		path = ""
	}
	path = "/" + path
	if deployment == "" {
		http.NotFound(w, r)
		return
	}

	apiKey, authenticated := f.resolveAPIKey(r)
	if apiKey == "" && f.singleKeyFallback {
		// unauthenticated: OSS serves public routes when the worker fleet
		// belongs to a single key
		apiKey, _ = f.registry.SingleAPIKey()
	}
	if apiKey == "" {
		w.Header().Set("WWW-Authenticate", "Bearer")
		f.writeMiss(w, http.StatusUnauthorized, MissUnauthenticated, "authentication required")
		return
	}

	candidates := f.registry.Candidates(apiKey, agentName, deployment)
	if len(candidates) == 0 && f.fallback == nil {
		f.writeUnavailable(w, "no workers available for deployment")
		return
	}

	// manifest match across the deployment's workers: FULL wins; PARTIAL only
	// yields 405 when nothing matches fully.
	matchAll := func(p string) (matched []*Registration, route *Route, partial, restricted bool) {
		for _, reg := range candidates {
			rt, res := reg.Manifest.Match(p, r.Method)
			switch res {
			case MatchFull:
				if !authenticated && !rt.Public {
					restricted = true
					continue
				}
				if route == nil {
					route = rt
				}
				matched = append(matched, reg)
			case MatchPartial:
				partial = true
			}
		}
		return
	}

	matched, route, partial, restricted := matchAll(path)
	// no exact match: if only the trailing-slash alternate matches a registered
	// route, normalize the path to that form and serve it directly (no client
	// redirect). The exact form is tried first, so a route registered with a
	// trailing slash is served as-is; this only rewrites a slash mismatch toward
	// the registered form. When the request must be relayed, the serving node
	// runs this same normalization, so no redirect is ever emitted.
	if route == nil && !partial && !restricted {
		for _, reg := range candidates {
			if alt, ok := reg.Manifest.slashAlternate(path, r.Method); ok {
				path = alt
				matched, route, partial, restricted = matchAll(path)
				break
			}
		}
	}
	if route == nil && f.fallback != nil {
		// nothing local can serve: hand off before the local status mapping
		if f.fallback(w, r, &FallbackRequest{
			APIKey: apiKey, Authenticated: authenticated,
			AgentName: agentName, Deployment: deployment, Method: r.Method, Path: path,
		}) {
			return
		}
		if len(candidates) == 0 && !restricted && !partial {
			f.writeUnavailable(w, "no workers available for deployment")
			return
		}
	}
	if route == nil {
		switch {
		case restricted:
			w.Header().Set("WWW-Authenticate", "Bearer")
			f.writeMiss(w, http.StatusUnauthorized, MissUnauthenticated, "authentication required")
		case partial:
			f.writeMiss(w, http.StatusMethodNotAllowed, MissMethodNotAllowed, "method not allowed")
		default:
			f.writeMiss(w, http.StatusNotFound, MissNotFound, "not found")
		}
		return
	}

	bodyConsumed := int64(0)
	countingBody := &countingReader{r: r.Body, n: &bodyConsumed}

	attempted := make(map[*Registration]bool)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		reg := pickWorker(matched, attempted)
		if reg == nil {
			break
		}
		attempted[reg] = true

		done, retryable := f.bridge(w, r, reg, path, countingBody, bodyConsumed)
		if done || !retryable {
			return
		}
	}

	// the route matched locally but nothing served it (matches draining or
	// conn-less, or every attempt failed before writing): the fallback may hold
	// capacity elsewhere. Safe exactly while no request bytes were consumed -
	// reaching this point implies it, since consuming attempts are never
	// retryable.
	if bodyConsumed == 0 && f.fallback != nil {
		if f.fallback(w, r, &FallbackRequest{
			APIKey: apiKey, Authenticated: authenticated,
			AgentName: agentName, Deployment: deployment, Method: r.Method, Path: path,
		}) {
			return
		}
	}

	f.writeUnavailable(w, "no worker could serve the request")
}

// pickWorker chooses a worker by the power of two choices: sample two eligible
// registrations at random and take the one with fewer in-flight streams (least
// outstanding requests). This approximates optimal load spreading without global
// coordination or the herding of exact least-loaded, and - unlike a
// self-reported load - the in-flight count is observed here from the worker's
// own session. Eligible = not already attempted, has a live session, not draining.
func pickWorker(regs []*Registration, ignore map[*Registration]bool) *Registration {
	eligible := regs[:0:0]
	for _, reg := range regs {
		if ignore[reg] || !reg.HasSession() {
			continue
		}
		if reg.Draining != nil && reg.Draining() {
			continue
		}
		eligible = append(eligible, reg)
	}
	if len(eligible) == 0 {
		return nil
	}
	return eligible[p2c(len(eligible), func(i int) int { return eligible[i].InflightStreams() })]
}

// p2c returns the index of the less-loaded of two distinct random draws from
// [0,n) (n >= 1). With n == 2 both are always sampled, so it is exact; larger n
// trades a little optimality for O(1) work and no herding.
func p2c(n int, load func(int) int) int {
	if n == 1 {
		return 0
	}
	i := rand.IntN(n)
	j := rand.IntN(n - 1)
	if j >= i { // fold to a distinct second draw
		j++
	}
	if load(i) <= load(j) {
		return i
	}
	return j
}

// bridge runs one attempt against one worker. done means a response (or abort)
// reached the client; retryable reports whether another attempt is safe per the
// retry table: idempotent/bodyless until any response byte arrived,
// anything on HSR_REFUSED, nothing once bytes were consumed otherwise.
func (f *Front) bridge(
	w http.ResponseWriter,
	r *http.Request,
	reg *Registration,
	path string,
	body io.Reader,
	bodyConsumedBefore int64,
) (done bool, retryable bool) {
	ctx := r.Context()
	stream, err := reg.OpenStream(ctx)
	if err != nil {
		return false, true // no session/capacity here; try another worker
	}
	defer stream.Close()

	stop := context.AfterFunc(ctx, func() {
		stream.Reset(ResetCancel, "client disconnected")
	})
	defer stop()

	// serialize the request into the stream concurrently with response reading:
	// directions are independent (full duplex within the stream)
	outReq := f.outboundRequest(r, path, body)
	writeErrCh := make(chan error, 1)
	go func() {
		err := outReq.Write(stream)
		if err == nil {
			err = stream.CloseWrite()
		} else {
			// fail fast: the worker is waiting for bytes that will never come
			stream.Reset(ResetCancel, "request write failed")
		}
		writeErrCh <- err
	}()

	counted := &countingReader{r: stream, n: new(int64)}
	br := responseReadPool.Get().(*bufio.Reader)
	br.Reset(counted)
	// bridge returns only after the response is fully drained, so the reader is
	// free to recycle here; Reset(nil) drops the stream reference so the pool
	// never pins a dead conn.
	defer func() { br.Reset(nil); responseReadPool.Put(br) }()

	resp, err := f.readResponseHead(w, br, outReq, stream)
	if err != nil {
		retryable = f.classifyRetry(r, stream, *counted.n, bodyConsumedBefore, err)
		if !retryable {
			f.logger.Warnw("agent endpoint request failed", err,
				"workerID", reg.WorkerID, "path", path)
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return true, false
		}
		// join the request writer before another attempt touches the shared
		// body reader (retries are bodyless per the table, so this is prompt)
		stream.Reset(ResetCancel, "retrying elsewhere")
		<-writeErrCh
		return false, true
	}

	// a response byte arrived: from here every failure is surfaced, never retried
	copyResponseHeaders(w.Header(), resp)
	w.WriteHeader(resp.StatusCode)

	rc := http.NewResponseController(w)
	bufp := copyBufferPool.Get().(*[]byte)
	buf := *bufp
	defer copyBufferPool.Put(bufp)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				stream.Reset(ResetCancel, "client write failed")
				return true, false
			}
			_ = rc.Flush()
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			// never expose a clean-looking short body
			select {
			case werr := <-writeErrCh:
				f.logger.Debugw("request write result after response failure", "error", werr)
			default:
			}
			panic(http.ErrAbortHandler)
		}
	}
	return true, false
}

// readResponseHead reads the worker's response head, relaying 1xx informational
// responses to the client and returning the final head. A 101 is not treated as
// informational (there is no protocol upgrade over an opaque request stream): it
// is returned as-is as the final response rather than looped past.
func (f *Front) readResponseHead(w http.ResponseWriter, br *bufio.Reader, outReq *http.Request, stream Stream) (*http.Response, error) {
	deadline := time.NewTimer(responseHeadTimeout)
	defer deadline.Stop()
	headCh := make(chan struct{})
	go func() {
		select {
		case <-deadline.C:
			stream.Reset(ResetCancel, "response head timeout")
		case <-headCh:
		}
	}()
	defer close(headCh)

	for {
		resp, err := http.ReadResponse(br, outReq)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode >= 100 && resp.StatusCode < 200 && resp.StatusCode != http.StatusSwitchingProtocols {
			// informational: relay and keep reading
			addHeaders(w.Header(), resp.Header)
			w.WriteHeader(resp.StatusCode)
			clear(w.Header())
			continue
		}
		return resp, nil
	}
}

// classifyRetry implements the retry table.
func (f *Front) classifyRetry(r *http.Request, stream Stream, responseBytes, bodyConsumedBefore int64, err error) bool {
	if responseBytes > 0 || stream.BytesRead() > 0 {
		return false
	}
	if stream.Refused() {
		// the worker proved non-dispatch; safe for any method, but only when the
		// request body can be replayed (nothing consumed yet)
		return bodyConsumedBefore == 0 && r.ContentLength == 0
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return r.ContentLength == 0
	}
	return false
}

// outboundRequest builds the request serialized into the stream: the path the
// worker's router sees (deployment prefix stripped), hop-by-hop headers removed,
// forwarding headers appended.
func (f *Front) outboundRequest(r *http.Request, path string, body io.Reader) *http.Request {
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.URL = &url.URL{Path: path, RawQuery: r.URL.RawQuery}
	out.Host = r.Host
	out.Body = io.NopCloser(body)
	// one exchange per stream: closing the worker-local app connection after the
	// response is what lets the opaque pump observe the end of the exchange and
	// free the stream slot
	out.Close = true

	removeHopByHopHeaders(out.Header)
	out.Header.Del("Expect") // the front owns 100-continue semantics client-side

	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		prior := out.Header.Get("X-Forwarded-For")
		if prior != "" {
			out.Header.Set("X-Forwarded-For", prior+", "+host)
		} else {
			out.Header.Set("X-Forwarded-For", host)
		}
	}
	return out
}

// hop-by-hop headers per RFC 9110; Connection-nominated headers are dropped too.
func removeHopByHopHeaders(h http.Header) {
	for _, f := range h.Values("Connection") {
		for _, sf := range strings.Split(f, ",") {
			if sf = strings.TrimSpace(sf); sf != "" {
				h.Del(sf)
			}
		}
	}
	for _, k := range []string{
		"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
		"Te", "Trailer", "Transfer-Encoding", "Upgrade",
	} {
		h.Del(k)
	}
}

// addHeaders copies every value of every header from src into dst.
func addHeaders(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func copyResponseHeaders(dst http.Header, resp *http.Response) {
	addHeaders(dst, resp.Header)
	removeHopByHopHeaders(dst)
	if resp.ContentLength >= 0 && dst.Get("Content-Length") == "" {
		dst.Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}
}

type countingReader struct {
	r io.Reader
	n *int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	*c.n += int64(n)
	return n, err
}

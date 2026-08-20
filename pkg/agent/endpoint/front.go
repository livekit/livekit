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
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/utils/guid"
	"github.com/livekit/psrpc"
)

const (
	// PathPrefix is the public route namespace: /agents/{deployment}/{path...}
	PathPrefix = "/agents/"

	// responseHeadTimeout bounds the wait for the worker's response head. Bodies
	// (SSE, long streams) are unbounded; the head never legitimately takes this
	// long.
	responseHeadTimeout = 90 * time.Second

	// maxAttempts bounds worker retries per request
	maxAttempts = 3
)

// ScopeResolver maps an inbound request to its project scope. It returns the
// api key the request is authorized for (empty when unauthenticated) - the
// service layer implements it from validated grants.
type ScopeResolver func(r *http.Request) (apiKey string, authenticated bool)

type Front struct {
	registry     *Registry
	resolveScope ScopeResolver
	logger       logger.Logger

	// remote resolves and relays requests this node cannot serve; nil on
	// single-node deployments. A getter so enabling multi-node after the front
	// is mounted still takes effect.
	remote func() *Remote
	// relayMode fronts serve the relay listener on the private network: scope
	// comes from trusted relay headers and requests are served from local
	// registrations only (a miss is final, never re-relayed)
	relayMode bool
	// see WithSingleKeyFallback
	singleKeyFallback bool
}

func NewFront(registry *Registry, resolveScope ScopeResolver, log logger.Logger) *Front {
	return &Front{
		registry:     registry,
		resolveScope: resolveScope,
		logger:       log.WithComponent("agents.endpoint"),
	}
}

// WithRemote enables multi-node (and cross-region) resolution and relaying.
func (f *Front) WithRemote(remote func() *Remote) *Front {
	f.remote = remote
	return f
}

// WithSingleKeyFallback resolves unauthenticated requests to the registry's
// single api key when the scope resolver yields none. Self-hosted convenience
// only: a multi-tenant front must never guess a scope from what happens to be
// registered.
func (f *Front) WithSingleKeyFallback() *Front {
	f.singleKeyFallback = true
	return f
}

func (f *Front) getRemote() *Remote {
	if f.remote == nil {
		return nil
	}
	return f.remote()
}

// NewRelayFront serves the node's relay listener.
func NewRelayFront(registry *Registry, log logger.Logger) *Front {
	return &Front{
		registry:  registry,
		relayMode: true,
		logger:    log.WithComponent("agents.endpoint.relay"),
	}
}

func (f *Front) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest, ok := strings.CutPrefix(r.URL.Path, PathPrefix)
	if !ok {
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

	isWS := isWebSocketUpgrade(r)

	var apiKey string
	var authenticated bool
	if f.relayMode {
		// origin already resolved scope; the relay listener is only reachable
		// on the private network
		apiKey = r.Header.Get(relayHeaderScope)
		authenticated = r.Header.Get(relayHeaderAuthed) == "1"
		stripRelayHeaders(r.Header)
		if apiKey == "" {
			http.Error(w, "missing relay scope", http.StatusBadGateway)
			return
		}
	} else {
		// never trust relay headers on the public front
		stripRelayHeaders(r.Header)
		apiKey, authenticated = f.resolveScope(r)
		if apiKey == "" && f.singleKeyFallback {
			// unauthenticated: OSS serves public routes when the worker fleet
			// belongs to a single key
			apiKey, _ = f.registry.SingleAPIKey()
		}
		if apiKey == "" {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
	}

	remote := f.getRemote()
	candidates := f.registry.Candidates(apiKey, deployment)
	if len(candidates) == 0 && (remote == nil || f.relayMode) {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "no workers available for deployment", http.StatusServiceUnavailable)
		return
	}

	// manifest match across the deployment's workers: FULL wins; PARTIAL only
	// yields 405 when nothing matches fully; slash-redirect mirrors FastAPI
	var matched []*Registration
	var route *Route
	partial := false
	restricted := false
	for _, reg := range candidates {
		rt, res := reg.Manifest.Match(path, r.Method, isWS)
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
	if route == nil && remote != nil && !f.relayMode {
		// nothing local can serve: resolve across the fleet and relay once
		if f.remoteServe(w, r, remote, apiKey, authenticated, deployment, path, isWS) {
			return
		}
		if len(candidates) == 0 && !restricted && !partial {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "no workers available for deployment", http.StatusServiceUnavailable)
			return
		}
	}
	if route == nil {
		switch {
		case restricted:
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "authentication required", http.StatusUnauthorized)
		case partial:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		default:
			for _, reg := range candidates {
				if alt, ok := reg.Manifest.RedirectSlashes(path, r.Method, isWS); ok {
					u := *r.URL
					u.Path = PathPrefix + deployment + alt
					http.Redirect(w, r, u.String(), http.StatusTemporaryRedirect)
					return
				}
			}
			http.NotFound(w, r)
		}
		return
	}

	bodyConsumed := int64(0)
	countingBody := &countingReader{r: r.Body, n: &bodyConsumed}

	attempted := make(map[*Registration]bool)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		reg := pickWeighted(matched, attempted)
		if reg == nil {
			break
		}
		attempted[reg] = true

		done, retryable := f.bridge(w, r, reg, path, countingBody, bodyConsumed, isWS)
		if done || !retryable {
			return
		}
	}

	// the route matched locally but nothing served it (matches draining or
	// conn-less, or every attempt failed before writing): another node may hold
	// capacity. Safe exactly while no request bytes were consumed - reaching
	// this point implies it, since consuming attempts are never retryable.
	if bodyConsumed == 0 && remote != nil && !f.relayMode {
		if f.remoteServe(w, r, remote, apiKey, authenticated, deployment, path, isWS) {
			return
		}
	}

	w.Header().Set("Retry-After", "1")
	http.Error(w, "no worker could serve the request", http.StatusServiceUnavailable)
}

// pickWeighted is capacity-weighted random over non-draining workers with
// attached data connections.
func pickWeighted(regs []*Registration, ignore map[*Registration]bool) *Registration {
	var sum float32
	weights := make([]float32, len(regs))
	for i, reg := range regs {
		if ignore[reg] || reg.AttachedConns() == 0 {
			continue
		}
		if reg.Draining != nil && reg.Draining() {
			continue
		}
		w := float32(1)
		if reg.Load != nil {
			w = max(0.01, 1-reg.Load())
		}
		weights[i] = w
		sum += w
	}
	if sum == 0 {
		return nil
	}
	target := rand.Float32() * sum
	for i, reg := range regs {
		if target -= weights[i]; weights[i] > 0 && target <= 0 {
			return reg
		}
	}
	return nil
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
	isWS bool,
) (done bool, retryable bool) {
	conn := reg.PickConn()
	if conn == nil {
		return false, true // no capacity here; try another worker
	}

	stream, err := conn.OpenStream(&livekit.AgentHttp_HttpStreamOpen{
		RequestId:  guid.New("AER_"),
		ClientAddr: r.RemoteAddr,
	})
	if err != nil {
		return false, true
	}
	defer stream.Close()

	ctx := r.Context()
	stop := context.AfterFunc(ctx, func() {
		stream.Reset(livekit.AgentHttp_HSR_CANCEL, "client disconnected")
	})
	defer stop()

	// serialize the request into the stream concurrently with response reading:
	// directions are independent (full duplex within the stream)
	outReq := f.outboundRequest(r, path, body, isWS)
	writeErrCh := make(chan error, 1)
	if isWS {
		// upgrade requests have no body: write the head inline and do NOT
		// half-close - the client->worker direction carries the session, and
		// the upgrade pump must be the stream's only writer
		if err := outReq.Write(stream); err != nil {
			stream.Reset(livekit.AgentHttp_HSR_CANCEL, "request write failed")
			return false, true
		}
		writeErrCh <- nil
	} else {
		go func() {
			err := outReq.Write(stream)
			if err == nil {
				err = stream.CloseWrite()
			} else {
				// fail fast: the worker is waiting for bytes that will never come
				stream.Reset(livekit.AgentHttp_HSR_CANCEL, "request write failed")
			}
			writeErrCh <- err
		}()
	}

	counted := &countingReader{r: stream, n: new(int64)}
	br := bufio.NewReader(counted)

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
		stream.Reset(livekit.AgentHttp_HSR_CANCEL, "retrying elsewhere")
		<-writeErrCh
		return false, true
	}

	// a response byte arrived: from here every failure is surfaced, never retried
	if resp.StatusCode == http.StatusSwitchingProtocols {
		f.bridgeUpgrade(w, resp, br, stream)
		return true, false
	}

	copyResponseHeaders(w.Header(), resp)
	w.WriteHeader(resp.StatusCode)

	rc := http.NewResponseController(w)
	buf := make([]byte, 32<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				stream.Reset(livekit.AgentHttp_HSR_CANCEL, "client write failed")
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

// readResponseHead reads the worker's response head, relaying informational
// responses (1xx except 101) to the client.
func (f *Front) readResponseHead(w http.ResponseWriter, br *bufio.Reader, outReq *http.Request, stream *Stream) (*http.Response, error) {
	deadline := time.NewTimer(responseHeadTimeout)
	defer deadline.Stop()
	headCh := make(chan struct{})
	go func() {
		select {
		case <-deadline.C:
			stream.Reset(livekit.AgentHttp_HSR_CANCEL, "response head timeout")
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
			for k, vv := range resp.Header {
				for _, v := range vv {
					w.Header().Add(k, v)
				}
			}
			w.WriteHeader(resp.StatusCode)
			clear(w.Header())
			continue
		}
		return resp, nil
	}
}

// bridgeUpgrade hijacks the client connection after a 101 and pumps raw bytes in
// both directions; the stream carries the rest of the WebSocket session.
func (f *Front) bridgeUpgrade(w http.ResponseWriter, resp *http.Response, br *bufio.Reader, stream *Stream) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		// e.g. HTTP/2 client conns cannot be upgraded
		stream.Reset(livekit.AgentHttp_HSR_CANCEL, "client does not support upgrade")
		http.Error(w, "upgrade not supported on this connection", http.StatusBadGateway)
		return
	}
	clientConn, clientRW, err := hj.Hijack()
	if err != nil {
		stream.Reset(livekit.AgentHttp_HSR_CANCEL, "hijack failed")
		return
	}
	defer clientConn.Close()

	if err := resp.Write(clientRW); err != nil {
		return
	}
	if err := clientRW.Flush(); err != nil {
		return
	}

	errCh := make(chan error, 2)
	go func() {
		// worker -> client, including bytes the bufio reader already buffered
		_, err := io.Copy(clientConn, br)
		errCh <- err
	}()
	go func() {
		// client -> worker
		buf := make([]byte, 32<<10)
		for {
			n, rerr := clientRW.Read(buf)
			if n > 0 {
				if _, werr := stream.Write(buf[:n]); werr != nil {
					errCh <- werr
					return
				}
			}
			if rerr != nil {
				_ = stream.CloseWrite()
				errCh <- rerr
				return
			}
		}
	}()
	<-errCh
	stream.Reset(livekit.AgentHttp_HSR_CANCEL, "upgrade session ended")
}

// classifyRetry implements the retry table.
func (f *Front) classifyRetry(r *http.Request, stream *Stream, responseBytes, bodyConsumedBefore int64, err error) bool {
	if responseBytes > 0 || stream.BytesRead() > 0 {
		return false
	}
	if stream.Refused() {
		// the worker proved non-dispatch; safe for any method, but only when the
		// request body can be replayed (nothing consumed yet)
		return bodyConsumedBefore == 0 && r.ContentLength == 0
	}
	if errors.Is(err, ErrStreamRefused) {
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
func (f *Front) outboundRequest(r *http.Request, path string, body io.Reader, isWS bool) *http.Request {
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.URL = &url.URL{Path: path, RawQuery: r.URL.RawQuery}
	out.Host = r.Host
	out.Body = io.NopCloser(body)
	// one exchange per stream: closing the worker-local app connection after the
	// response is what lets the opaque pump observe the end of the exchange and
	// free the stream slot
	out.Close = !isWS

	removeHopByHopHeaders(out.Header, isWS)
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
// For WebSocket upgrades Connection/Upgrade survive so the worker-side bridge
// sees a real upgrade request.
func removeHopByHopHeaders(h http.Header, isWS bool) {
	for _, f := range h.Values("Connection") {
		for _, sf := range strings.Split(f, ",") {
			if sf = strings.TrimSpace(sf); sf != "" && !strings.EqualFold(sf, "upgrade") {
				h.Del(sf)
			}
		}
	}
	for _, k := range []string{
		"Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
		"Te", "Trailer", "Transfer-Encoding",
	} {
		h.Del(k)
	}
	if !isWS {
		h.Del("Connection")
		h.Del("Upgrade")
	} else {
		h.Set("Connection", "Upgrade")
	}
}

func copyResponseHeaders(dst http.Header, resp *http.Response) {
	for k, vv := range resp.Header {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
	removeHopByHopHeaders(dst, false)
	if resp.ContentLength >= 0 && dst.Get("Content-Length") == "" {
		dst.Set("Content-Length", fmt.Sprintf("%d", resp.ContentLength))
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

// remoteServe resolves across the fleet and relays to the winner. It reports
// whether the response was written; a resolve miss falls back to the local
// status mapping.
func (f *Front) remoteServe(w http.ResponseWriter, r *http.Request, remote *Remote, scope string, authenticated bool, deployment, path string, isWS bool) bool {
	req := &rpc.ResolveEndpointRequest{
		Scope:         scope,
		Deployment:    deployment,
		Path:          path,
		Method:        r.Method,
		Websocket:     isWS,
		Authenticated: authenticated,
	}
	resp, err := remote.Resolve(r.Context(), req)
	if err != nil {
		// a node holding the deployment answered with a typed miss: surface the
		// precise status; bus-level failures fall back to the local mapping
		var perr psrpc.Error
		if errors.As(err, &perr) {
			switch perr.Code() {
			case psrpc.NotFound:
				http.NotFound(w, r)
				return true
			case psrpc.Unauthenticated:
				w.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(w, "authentication required", http.StatusUnauthorized)
				return true
			case psrpc.FailedPrecondition:
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return true
			case psrpc.Unavailable:
				w.Header().Set("Retry-After", "1")
				http.Error(w, "no worker capacity for the endpoint", http.StatusServiceUnavailable)
				return true
			}
		}
		return false
	}

	remote.relay(w, r, resp, scope, authenticated)
	return true
}

func stripRelayHeaders(h http.Header) {
	h.Del(relayHeaderScope)
	h.Del(relayHeaderAuthed)
}

func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		httpHeaderContainsToken(r.Header, "Connection", "upgrade")
}

func httpHeaderContainsToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, f := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(f), token) {
				return true
			}
		}
	}
	return false
}

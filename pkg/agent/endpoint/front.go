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
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/livekit/livekit-server/pkg/agent/endpoint/router"
	"github.com/livekit/livekit-server/pkg/agent/endpoint/wire"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

const (
	// PathPrefix is the public route namespace: /agents/{agent_name}/{deployment}/{path...}
	PathPrefix = "/agents/"

	// MaxPathLength caps the escaped route path. Matching runs before the
	// request head is sized, so this is the only bound on it.
	MaxPathLength = 8 << 10

	// responseHeadTimeout bounds the wait for the worker's response head. Bodies
	// (SSE, long streams) are unbounded; the head never legitimately takes this
	// long.
	responseHeadTimeout = 90 * time.Second

	// maxAttempts bounds worker retries per request
	maxAttempts = 3

	// maxRequestIDLen bounds the client's idempotence token: it reaches this
	// node's logs and the worker's, so it cannot be unbounded.
	maxRequestIDLen = 128

	// maxInformationalHeads bounds 1xx responses before the final head, so a
	// worker cannot hold a client open by trickling them forever.
	maxInformationalHeads = 8

	maxResponseHeadSize = 1 << 20
	responseBufSize     = 8 << 10
)

var (
	errNotEndpointPath      = errors.New("endpoint: not an agent endpoint path")
	errMalformedPath        = errors.New("endpoint: malformed agent endpoint path")
	errPathTooLong          = errors.New("endpoint: agent endpoint path too long")
	errRequestHeadTooLarge  = errors.New("endpoint: request head too large")
	errProtocolSwitch       = errors.New("endpoint: worker switched protocols on an HTTP exchange")
	errTooManyInformational = errors.New("endpoint: too many informational responses")
	errBadStatus            = errors.New("endpoint: response status out of range")
	errHeadTooLarge         = errors.New("endpoint: response head too large")
)

// AccessLevel is how far a request's caller is trusted. Callers compare against
// it, so a new level must be inserted at its correct rank.
type AccessLevel int

const (
	// AccessNone presented no credential.
	AccessNone AccessLevel = iota
	// AccessCredentialed presented a valid token carrying no agent-endpoint
	// grant for the addressed agent and deployment.
	AccessCredentialed
	// AccessGranted presented a token whose agent-endpoint grant covers the
	// addressed agent and deployment.
	AccessGranted
)

func (a AccessLevel) String() string {
	switch a {
	case AccessNone:
		return "none"
	case AccessCredentialed:
		return "credentialed"
	case AccessGranted:
		return "granted"
	default:
		return fmt.Sprintf("%d", int(a))
	}
}

// Access is what the front knows about a request's caller, together with the
// serving state it resolved to. The front never keys anything itself: whatever
// scopes a request - a tenant, an api key, a project - is resolved by the
// embedder and arrives here already looked up.
type Access struct {
	// Scope is the deployment's serving state this request is placed against.
	// nil means no worker here holds it; Fallback may still place it elsewhere.
	Scope *Scope
	// Fallback serves the request elsewhere (e.g. a multi-node relay), already
	// curried on the deployment it resolved. nil means local misses are final.
	Fallback Fallback
	Level    AccessLevel
}

// AccessResolver maps an inbound request, plus the agent and deployment its URL
// addresses, to the caller's access. ok is false when the request cannot be
// placed at all - no credential, or an unknown tenant - and the front
// challenges.
type AccessResolver func(r *http.Request, agentName, deployment string) (access Access, ok bool)

type Front struct {
	params FrontParams
	pools  *bridgePools
}

// Identity resolves the agent and deployment a request addresses. Reporting
// false leaves them to the URL.
type Identity func(r *http.Request) (agentName, deployment string, ok bool)

// FrontParams configures a Front. Fields are read on every request once the
// Front is serving, so none may change after construction.
type FrontParams struct {
	ResolveAccess AccessResolver
	Logger        logger.Logger
	Identity      Identity
}

func NewFront(params FrontParams) *Front {
	params.Logger = params.Logger.WithComponent("agents.endpoint")
	return &Front{params: params, pools: newBridgePools()}
}

// Fallback serves a request elsewhere (e.g. a multi-node relay); it reports
// whether a response was written. Returning false falls back to the local
// status mapping. It is curried on the deployment it was resolved for, so it
// carries no scope arguments.
type Fallback func(w http.ResponseWriter, r *http.Request, level AccessLevel) bool

// writeUnavailable writes a 503 with a Retry-After hint: no local worker can
// serve the request and no fallback placed it elsewhere.
func (f *Front) writeUnavailable(w http.ResponseWriter, msg string) {
	w.Header().Set("Retry-After", "1")
	http.Error(w, msg, http.StatusServiceUnavailable)
}

func (f *Front) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ep, err := splitEndpointPath(r.URL)
	if err != nil {
		if errors.Is(err, errMalformedPath) {
			http.Error(w, "bad request path", http.StatusBadRequest)
			return
		}
		if errors.Is(err, errPathTooLong) {
			http.Error(w, "uri too long", http.StatusRequestURITooLong)
			return
		}
		http.NotFound(w, r)
		return
	}
	agentName, deployment, path, escPath := ep.agentName, ep.deployment, ep.path, ep.escPath
	if f.params.Identity != nil {
		// must precede resolveAccess, which may consume its source headers
		if name, dep, ok := f.params.Identity(r); ok {
			agentName, deployment = name, dep
		}
	}

	reqID, ok := requestID(r)
	if !ok {
		http.Error(w, "invalid X-Request-Id", http.StatusBadRequest)
		return
	}

	access, ok := f.params.ResolveAccess(r, agentName, deployment)
	if !ok {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}

	tbl := access.Scope.routeTable()
	if tbl == nil && access.Fallback == nil {
		f.writeUnavailable(w, "no workers available for deployment")
		return
	}

	mask := methodMask(r.Method)
	granted := access.Level >= AccessGranted
	matched, partial, denied := f.matchDeployment(access.Scope, tbl, path, mask, granted)
	// no exact match: if only the trailing-slash alternate matches a registered
	// route, normalize the path to that form and serve it directly (no client
	// redirect). The exact form is tried first, so a route registered with a
	// trailing slash is served as-is; this only rewrites a slash mismatch toward
	// the registered form. When the request must be relayed, the serving node
	// runs this same normalization, so no redirect is ever emitted.
	if len(matched) == 0 && !partial && !denied {
		if alt, altEsc, ok := slashAlternatePaths(tbl, path, escPath, mask); ok {
			path, escPath = alt, altEsc
			matched, partial, denied = f.matchDeployment(access.Scope, tbl, path, mask, granted)
		}
	}
	if len(matched) == 0 && access.Fallback != nil {
		// nothing local matched: hand off to the multi-node fallback (relay to a
		// node holding the deployment) before the local status mapping. The
		// serving node's relay listener installs no fallback of its own, so a
		// relayed request is served or errored there and never re-relays.
		if access.Fallback(w, r, access.Level) {
			return
		}
		if tbl == nil && !denied && !partial {
			f.writeUnavailable(w, "no workers available for deployment")
			return
		}
	}
	if len(matched) == 0 {
		switch {
		case denied:
			// access does not vary across candidates, so one verdict covers them all
			if access.Level >= AccessCredentialed {
				http.Error(w, "forbidden", http.StatusForbidden)
			} else {
				w.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(w, "authentication required", http.StatusUnauthorized)
			}
		case partial:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
		return
	}

	var bodyConsumed atomic.Int64
	a := &attempt{
		req:       r,
		escPath:   escPath,
		target:    requestTarget(escPath, r.URL.RawQuery),
		requestID: reqID,
		granted:   granted,
		pools:     f.pools,
	}
	a.body = &countingReader{r: r.Body, n: &bodyConsumed}
	a.preamble = a.newPreamble()
	a.refreshTimeout()
	if err := a.buildHead(); err != nil {
		if errors.Is(err, errRequestHeadTooLarge) {
			http.Error(w, "request header fields too large", http.StatusRequestHeaderFieldsTooLarge)
			return
		}
		f.params.Logger.Debugw("agent endpoint rejected a request head", "error", err, "requestID", reqID)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	attempted := make(map[*Registration]bool)
	for range maxAttempts {
		picked := pickWorker(matched, attempted)
		if picked == nil {
			break
		}
		attempted[picked.reg] = true

		a.before = bodyConsumed.Load()
		a.refreshTimeout()
		// the preamble is re-serialized per attempt, so this reaches only the
		// worker it was set for
		a.preamble.Route = picked.raw
		switch f.bridge(w, a, picked.reg) {
		case bridgeDone:
			return
		case bridgeAbort:
			// the head is on the wire already, so nothing can report the failure in
			// band. This must reach net/http to abort the response; anything that
			// recovers it completes the body.
			panic(http.ErrAbortHandler)
		}
	}

	// the route matched locally but nothing served it (matches draining or
	// conn-less, or every attempt failed before writing): the fallback may hold
	// capacity elsewhere. Safe exactly while no request bytes were consumed -
	// reaching this point implies it, since consuming attempts are never
	// retryable.
	if bodyConsumed.Load() == 0 && access.Fallback != nil {
		if access.Fallback(w, r, access.Level) {
			return
		}
	}

	f.writeUnavailable(w, "no worker could serve the request")
}

// matchDeployment resolves a path against the deployment's merged route table.
// matched is the dispatch test: a route can be decided while nothing is left to
// serve it, and an undecidable table dispatches with no route at all.
func (f *Front) matchDeployment(scope *Scope, tbl *routeTable, path string, mask router.Mask, granted bool) (matched []routeWorker, partial, denied bool) {
	if tbl == nil {
		return nil, false, false
	}
	var res router.Result
	matched, _, res, denied = tbl.match(path, mask, granted)
	switch res {
	case router.ResultPartial:
		partial = true
	case router.ResultOverBudget:
		// no route was decided, so its Public flag is unknown and only a grant
		// can clear the request
		if !granted {
			denied = true
			break
		}
		for _, reg := range scope.Candidates() {
			matched = append(matched, routeWorker{reg: reg})
		}
	}
	return
}

// slashAlternatePaths looks for the trailing-slash alternate of path in the
// deployment's table, returning the decoded and escaped forms to retry. Both
// forms come out of the same transform.
func slashAlternatePaths(tbl *routeTable, path, escPath string, mask router.Mask) (string, string, bool) {
	if tbl == nil || path == "/" {
		return path, escPath, false
	}
	var alt, altEsc string
	if strings.HasSuffix(path, "/") {
		// %2F decodes to a slash without being a separator, so the alternate
		// applies only while the slash is literal in escPath too
		if !strings.HasSuffix(escPath, "/") {
			return path, escPath, false
		}
		alt, altEsc = strings.TrimSuffix(path, "/"), strings.TrimSuffix(escPath, "/")
	} else {
		alt, altEsc = path+"/", escPath+"/"
	}
	if !tbl.serves(alt, mask) {
		return path, escPath, false
	}
	return alt, altEsc, true
}

// endpointPath is a request split into its routing components.
type endpointPath struct {
	agentName  string
	deployment string
	// path is decoded, for manifest matching
	path string
	// escPath keeps the client's encoding, for the request line the worker gets
	escPath string
}

// splitEndpointPath splits /agents/{agent_name}/{deployment}/{path...} from the
// ESCAPED path. The worker is handed a re-serialized request line, so the target
// must keep the client's encoding: a decoded %3F or %2F re-emits as a real '?'
// or '/' and changes which resource the worker routes to.
func splitEndpointPath(u *url.URL) (endpointPath, error) {
	rest, ok := strings.CutPrefix(u.EscapedPath(), PathPrefix)
	if !ok {
		return endpointPath{}, errNotEndpointPath
	}
	rawAgentName, rest, found := strings.Cut(rest, "/")
	if !found || rawAgentName == "" {
		return endpointPath{}, errNotEndpointPath
	}
	rawDeployment, escPath, found := strings.Cut(rest, "/")
	if !found {
		escPath = ""
	}
	if rawDeployment == "" {
		return endpointPath{}, errNotEndpointPath
	}
	escPath = "/" + escPath

	if len(escPath) > MaxPathLength {
		return endpointPath{}, errPathTooLong
	}

	agentName, err1 := url.PathUnescape(rawAgentName)
	deployment, err2 := url.PathUnescape(rawDeployment)
	path, err3 := url.PathUnescape(escPath)
	if err1 != nil || err2 != nil || err3 != nil {
		return endpointPath{}, errMalformedPath
	}
	// "_" and "%5F" both land here; neither is a registrable name
	// (IsReservedAgentName)
	if agentName == UnnamedAgentSegment {
		agentName = ""
	}
	return endpointPath{agentName: agentName, deployment: deployment, path: path, escPath: escPath}, nil
}

// pickWorker chooses a worker by the power of two choices: sample two eligible
// declarations at random and take the one with fewer in-flight streams.
// Eligible = not already attempted, has a live session, not draining.
func pickWorker(workers []routeWorker, ignore map[*Registration]bool) *routeWorker {
	var eligible []routeWorker
	for _, w := range workers {
		if ignore[w.reg] || !w.reg.HasSession() || w.reg.IsDraining() {
			continue
		}
		eligible = append(eligible, w)
	}
	if len(eligible) == 0 {
		return nil
	}
	return &eligible[p2c(eligible, func(w routeWorker) int { return w.reg.InflightStreams() })]
}

// p2c returns the index of the less-loaded of two distinct random draws from
// items, which must be non-empty.
func p2c[T any](items []T, load func(T) int) int {
	n := len(items)
	if n == 1 {
		return 0
	}
	i := rand.IntN(n)
	j := rand.IntN(n - 1)
	if j >= i { // fold to a distinct second draw
		j++
	}
	if load(items[i]) <= load(items[j]) {
		return i
	}
	return j
}

// bridgeOutcome is what one attempt against one worker concluded.
type bridgeOutcome int

const (
	// nothing reached the client; another worker may still serve it
	bridgeRetry bridgeOutcome = iota
	// a response, or an error standing in for one, reached the client
	bridgeDone
	// the response was committed and cannot be completed
	bridgeAbort
)

// bridge runs one attempt against one worker.
func (f *Front) bridge(w http.ResponseWriter, a *attempt, reg *Registration) bridgeOutcome {
	ctx := a.req.Context()
	stream, err := reg.OpenStream(ctx)
	if err != nil {
		return bridgeRetry // no session/capacity here; try another worker
	}
	defer stream.Close()

	stop := context.AfterFunc(ctx, func() {
		stream.Reset(livekit.AgentHttp_HSR_ABORT, "client disconnected")
	})
	defer stop()

	// serialize the request into the stream concurrently with response reading:
	// directions are independent (full duplex within the stream)
	writeErrCh := make(chan error, 1)
	go func() {
		err := a.writeRequest(stream)
		if err == nil {
			err = stream.CloseWrite()
		} else {
			// fail fast: the worker is waiting for bytes that will never come
			stream.Reset(livekit.AgentHttp_HSR_ABORT, "request write failed")
		}
		writeErrCh <- err
	}()

	lim := &headLimiter{r: stream, n: maxResponseHeadSize}
	br := f.pools.getReader(lim)
	defer f.pools.putReader(br)

	resp, err := a.readResponse(w, br, lim, stream)
	if err != nil {
		err = completionError(err)
		if !a.retryable(err) {
			f.params.Logger.Warnw("agent endpoint request failed", err,
				"workerID", reg.WorkerID, "path", a.escPath, "requestID", a.requestID)
			writeGatewayError(w, err)
			return bridgeDone
		}
		// join the request writer before another attempt touches the shared
		// body reader (retries are bodyless per the table, so this is prompt)
		stream.Reset(livekit.AgentHttp_HSR_ABORT, "retrying elsewhere")
		<-writeErrCh
		return bridgeRetry
	}
	// resp.Body must not be Closed: net/http's Close drains whatever the head
	// declared and the body has not delivered, blocking on a stream that is
	// about to be reset. stream.Close owns the underlying resource.

	// a response head arrived: from here every failure is surfaced
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	a.committed = true

	rc := http.NewResponseController(w)
	bufp := f.pools.getBuf()
	buf := *bufp
	defer f.pools.putBuf(bufp)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				stream.Reset(livekit.AgentHttp_HSR_ABORT, "client write failed")
				return bridgeDone
			}
			_ = rc.Flush()
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			// never expose a clean-looking short body
			return f.aborted(rerr, reg, a, writeErrCh)
		}
	}
	// framing complete; the sender may still report a short body in trailers
	if ce := wire.CompletionFromTrailers(resp.Trailer); ce != nil {
		return f.aborted(ce, reg, a, writeErrCh)
	}
	return bridgeDone
}

// aborted logs why a committed response cannot be completed.
func (f *Front) aborted(err error, reg *Registration, a *attempt, writeErrCh <-chan error) bridgeOutcome {
	f.logAborted(err, reg, a)
	select {
	case werr := <-writeErrCh:
		f.params.Logger.Debugw("request write result after response failure", "error", werr)
	default:
	}
	return bridgeAbort
}

func (f *Front) logAborted(err error, reg *Registration, a *attempt) {
	var ce *wire.CompletionError
	if errors.As(err, &ce) {
		f.params.Logger.Infow("agent endpoint response aborted",
			"workerID", reg.WorkerID, "path", a.escPath, "requestID", a.requestID,
			"completion", string(ce.Completion), "reason", ce.Reason)
		return
	}
	f.params.Logger.Infow("agent endpoint response aborted",
		"workerID", reg.WorkerID, "path", a.escPath, "requestID", a.requestID, "error", err)
}

// completionError normalizes a failure into the protocol's outcome vocabulary.
// A peer reset code becomes the outcome; anything else leaves dispatch unknown.
func completionError(err error) error {
	var sre *StreamResetError
	if errors.As(err, &sre) {
		return &wire.CompletionError{Completion: wire.CompletionFromResetCode(sre.Code)}
	}
	return err
}

// writeGatewayError maps a terminal state to a status for a request whose
// response never reached the client.
func writeGatewayError(w http.ResponseWriter, err error) {
	var ce *wire.CompletionError
	if errors.As(err, &ce) && ce.Completion == wire.CompletionTimeout {
		http.Error(w, "gateway timeout", http.StatusGatewayTimeout)
		return
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		http.Error(w, "gateway timeout", http.StatusGatewayTimeout)
		return
	}
	http.Error(w, "bad gateway", http.StatusBadGateway)
}

// bridgePools holds the per-request scratch one Front reuses: a body buffer and
// the buffered reader http.ReadResponse parses through.
type bridgePools struct {
	bodyBuf sync.Pool
	readers sync.Pool
}

func newBridgePools() *bridgePools {
	return &bridgePools{
		bodyBuf: sync.Pool{New: func() any { b := make([]byte, wire.BodyChunkSize); return &b }},
		readers: sync.Pool{New: func() any { return bufio.NewReaderSize(nil, responseBufSize) }},
	}
}

func (p *bridgePools) getBuf() *[]byte  { return p.bodyBuf.Get().(*[]byte) }
func (p *bridgePools) putBuf(b *[]byte) { p.bodyBuf.Put(b) }

func (p *bridgePools) getReader(r io.Reader) *bufio.Reader {
	br := p.readers.Get().(*bufio.Reader)
	br.Reset(r)
	return br
}

// putReader drops the stream reference so a pooled reader never pins a dead one.
func (p *bridgePools) putReader(br *bufio.Reader) {
	br.Reset(nil)
	p.readers.Put(br)
}

// headLimiter caps the bytes a response head may make this node buffer. release
// lifts the cap once the head is parsed; the body behind it is unbounded.
type headLimiter struct {
	r io.Reader
	n int64 // remaining head budget; negative once the head has been parsed
}

func (h *headLimiter) Read(p []byte) (int, error) {
	if h.n == 0 {
		return 0, errHeadTooLarge
	}
	if h.n > 0 && int64(len(p)) > h.n {
		p = p[:h.n]
	}
	n, err := h.r.Read(p)
	if h.n > 0 {
		h.n -= int64(n)
	}
	return n, err
}

func (h *headLimiter) release() { h.n = -1 }

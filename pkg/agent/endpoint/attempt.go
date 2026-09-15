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
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/livekit/livekit-server/pkg/agent/endpoint/wire"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/utils/guid"
)

// attempt is the per-request state shared across worker attempts.
type attempt struct {
	req *http.Request
	// escPath is the percent-encoded path; a decoded %0A would forge a log line
	escPath string
	// target is the origin-form request target, still percent-encoded
	target    string
	route     *Route
	requestID string
	granted   bool
	body      io.Reader
	pools     *bridgePools
	preamble  *livekit.AgentHttp_StreamPreamble
	// the serialized canonical request head, built once and replayed verbatim
	head []byte
	// client body bytes consumed when this attempt began
	before int64
	// a status line reached the client, so nothing may be retried past here
	committed bool
}

// replayable reports whether the request body can be sent again from the start.
// writeRequest never reads the body when ContentLength is 0, so an attempt that
// passes here leaves the counter where it found it and the next one replays too.
func (a *attempt) replayable() bool {
	return a.before == 0 && a.req.ContentLength == 0
}

// retryable implements the retry table: nothing once a status line has reached
// the client; before that, proven non-dispatch is safe for any method, and an
// outcome where the application ran is never safe.
func (a *attempt) retryable(err error) bool {
	if a.committed || !a.replayable() {
		return false
	}
	var ce *wire.CompletionError
	if errors.As(err, &ce) {
		switch ce.Completion {
		case wire.CompletionRefused:
			return true
		case wire.CompletionTimeout, wire.CompletionProtocol, wire.CompletionUnknown:
			return idempotentMethod(a.req.Method)
		default:
			return false // the application ran
		}
	}
	// dispatch is unknown
	return idempotentMethod(a.req.Method)
}

// newPreamble carries what the worker cannot derive from the HTTP bytes and
// must not infer from them - authenticated above all.
func (a *attempt) newPreamble() *livekit.AgentHttp_StreamPreamble {
	scheme := "http"
	if a.req.TLS != nil {
		scheme = "https"
	}
	clientAddr, _, _ := net.SplitHostPort(a.req.RemoteAddr)

	p := &livekit.AgentHttp_StreamPreamble{
		Kind:       livekit.AgentHttp_AEK_HTTP,
		RequestId:  a.requestID,
		Authorized: a.granted,
		ClientAddr: clientAddr,
		Scheme:     scheme,
	}
	if a.route != nil && a.route.Template != nil {
		p.Route = a.route.Template.String()
	}
	return p
}

// requestHeader builds the header set the worker sees.
func (a *attempt) requestHeader() http.Header {
	r := a.req
	h := r.Header.Clone()
	if h == nil {
		h = http.Header{}
	}
	removeHopByHopHeaders(h)
	// the signalling namespace is the node's to write: a client able to set
	// x-lk-completion could tell the worker a body it cut short was whole
	wire.StripReservedHeaders(h)
	h.Del("Expect") // the front owns 100-continue semantics client-side

	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		if prior := h.Get("X-Forwarded-For"); prior != "" {
			h.Set("X-Forwarded-For", prior+", "+host)
		} else {
			h.Set("X-Forwarded-For", host)
		}
	}
	return h
}

// buildHead serializes the canonical request head. It is byte-identical across
// attempts, so it is built once; only the preamble's shrinking deadline varies.
func (a *attempt) buildHead() error {
	head, err := wire.BuildRequestHead(a.req.Method, a.target, a.req.Host, a.requestHeader(), bodyLength(a.req))
	if err != nil {
		return err
	}
	if len(head) > wire.MaxRequestHeadSize {
		return errRequestHeadTooLarge
	}
	a.head = head
	return nil
}

// refreshTimeout restates the remaining budget: the preamble is built once, but
// a retry must not hand the worker time already spent.
func (a *attempt) refreshTimeout() {
	deadline, ok := a.req.Context().Deadline()
	if !ok {
		return
	}
	ms := time.Until(deadline).Milliseconds()
	if ms < 1 {
		ms = 1 // 0 means "no deadline", which an expired budget is not
	}
	a.preamble.TimeoutMs = uint32(ms)
}

// writeRequest writes the preamble, then the canonical head and body as opaque
// HTTP/1.1. A failure reading the client's body still ends the body explicitly
// where the framing allows it.
func (a *attempt) writeRequest(w io.Writer) error {
	if err := wire.WritePreamble(w, a.preamble); err != nil {
		return err
	}
	if _, err := w.Write(a.head); err != nil {
		return err
	}
	cl := bodyLength(a.req)
	if a.body == nil || cl == 0 {
		return nil
	}

	// a declared length is framed as declared; only an unknown length needs
	// chunking, the one framing that can carry a reason for a short body.
	var bw wire.BodyWriter
	if cl > 0 {
		bw = wire.NewIdentityBody(w)
	} else {
		bw = wire.NewChunkedBody(w)
	}

	bufp := a.pools.getBuf()
	defer a.pools.putBuf(bufp)

	n, err := wire.CopyBody(bw, a.body, *bufp)
	var srcErr *wire.SourceError
	switch {
	case errors.As(err, &srcErr):
		return bw.Close(wire.CompletionPeerGone, srcErr.Err.Error())
	case err != nil:
		return err
	case cl > 0 && n < cl:
		return bw.Close(wire.CompletionTruncated, fmt.Sprintf("declared %d bytes, read %d", cl, n))
	}
	return bw.Close(wire.CompletionUnknown, "")
}

// readResponse reads the worker's response head, relaying any informational
// heads that precede it. 1xx carries no body, so each returns as soon as it
// lands.
func (a *attempt) readResponse(
	w http.ResponseWriter,
	br *bufio.Reader,
	lim *headLimiter,
	stream Stream,
) (*http.Response, error) {
	// A deadline only bounds waiting, so failing to set or clear one must never
	// discard a response the peer has already delivered: a transport reports an
	// error here as soon as the stream is closed, which a worker that answered
	// in full and hung up has done.
	_ = stream.SetReadDeadline(time.Now().Add(responseHeadTimeout))
	for i := 0; ; i++ {
		if i > maxInformationalHeads {
			return nil, errTooManyInformational
		}
		resp, err := http.ReadResponse(br, a.req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode < 100 || resp.StatusCode > 599 {
			return nil, errBadStatus
		}
		if resp.StatusCode == http.StatusSwitchingProtocols {
			// no endpoint kind performs a protocol switch yet
			return nil, errProtocolSwitch
		}
		if resp.StatusCode >= 200 {
			lim.release()
			// bodies (SSE, long streams) are unbounded; only the head is deadlined
			_ = stream.SetReadDeadline(time.Time{})
			return resp, nil
		}
		h := w.Header()
		copyResponseHeaders(h, resp.Header)
		w.WriteHeader(resp.StatusCode)
		clear(h)
		a.committed = true
	}
}

// requestID resolves the request's idempotence token. It is client-chosen, so it
// is never rewritten - only accepted, or refused so the caller can retry under a
// token the front will honor. Being client-chosen it is neither unique nor
// trusted: two callers may present the same value, so log queries scope by
// worker and deployment.
func requestID(r *http.Request) (string, bool) {
	v := r.Header.Values("X-Request-Id")
	switch {
	case len(v) == 0:
		return guid.New("AER_"), true
	case len(v) > 1:
		return "", false // a token naming two requests dedups neither
	}
	if !validRequestID(v[0]) {
		return "", false
	}
	return v[0], true
}

// validRequestID accepts visible ASCII within the length bound, so UUID, ULID,
// base64 and base64url pass unchanged while control characters do not.
func validRequestID(id string) bool {
	if len(id) == 0 || len(id) > maxRequestIDLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] < 0x21 || id[i] > 0x7e {
			return false
		}
	}
	return true
}

func idempotentMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	}
	return false
}

// bodyLength is the framing the head declares: a known length, 0 for no body,
// or -1 when only the client knows where it ends.
func bodyLength(r *http.Request) int64 {
	if r.Body == nil || r.Body == http.NoBody {
		return 0
	}
	return r.ContentLength
}

func requestTarget(escPath, rawQuery string) string {
	if rawQuery == "" {
		return escPath
	}
	return escPath + "?" + rawQuery
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

// copyResponseHeaders forwards what the worker sent, minus hop-by-hop fields (a
// forwarded Transfer-Encoding makes the client de-chunk an already-decoded body)
// and minus the reserved namespace, which is not the client's to see.
func copyResponseHeaders(dst, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
	removeHopByHopHeaders(dst)
	wire.StripReservedHeaders(dst)
}

// countingReader tracks how much of the client body has been consumed. The
// counter is atomic because it is read while the request writer goroutine holds
// the reader.
type countingReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.n.Add(int64(n))
	}
	return n, err
}

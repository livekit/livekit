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

package wire

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/net/http/httpguts"

	"github.com/livekit/protocol/livekit"
)

const (
	// MaxPreambleSize bounds the one allocation a peer can size before any HTTP
	// bytes are parsed.
	MaxPreambleSize = 64 << 10

	MaxRequestHeadSize = 1 << 20
	BodyChunkSize      = 32 << 10

	// ReservedHeaderPrefix namespaces the in-band signalling below.
	ReservedHeaderPrefix = "x-lk-"

	completionHeader = "x-lk-completion"
	errorHeader      = "x-lk-error"
)

// Completion is how one direction of an exchange ended, spelled as the literal
// x-lk-completion trailer token. The empty value carries no information, so a
// sender that omits the field cannot thereby claim success; success is the
// body's own framing reaching its end and is never announced.
type Completion string

const (
	CompletionUnknown   Completion = ""
	CompletionTruncated Completion = "truncated"
	CompletionPeerGone  Completion = "peer_gone"
	CompletionRefused   Completion = "refused"
	CompletionInternal  Completion = "internal"
	CompletionTimeout   Completion = "timeout"
	CompletionCanceled  Completion = "canceled"
	CompletionProtocol  Completion = "protocol"
)

// CompletionError is a terminal state other than success.
type CompletionError struct {
	Completion Completion
	Reason     string
}

func (e *CompletionError) Error() string {
	c := string(e.Completion)
	if c == "" {
		c = "unknown"
	}
	if e.Reason == "" {
		return fmt.Sprintf("endpoint: exchange ended %s", c)
	}
	return fmt.Sprintf("endpoint: exchange ended %s: %s", c, e.Reason)
}

// ResetCode is the QUIC reset code carrying this completion when it happened
// before any HTTP bytes flowed. Outcomes that can only arise mid-body have no
// code of their own and collapse to HSR_ABORT.
func (c Completion) ResetCode() livekit.AgentHttp_HttpStreamResetCode {
	switch c {
	case CompletionRefused:
		return livekit.AgentHttp_HSR_REFUSED
	case CompletionInternal:
		return livekit.AgentHttp_HSR_INTERNAL
	case CompletionTimeout:
		return livekit.AgentHttp_HSR_TIMEOUT
	case CompletionProtocol:
		return livekit.AgentHttp_HSR_PROTOCOL
	default:
		return livekit.AgentHttp_HSR_ABORT
	}
}

// CompletionFromResetCode is the inverse. HSR_ABORT carries no claim about
// whether the request was applied.
func CompletionFromResetCode(code livekit.AgentHttp_HttpStreamResetCode) Completion {
	switch code {
	case livekit.AgentHttp_HSR_REFUSED:
		return CompletionRefused
	case livekit.AgentHttp_HSR_INTERNAL:
		return CompletionInternal
	case livekit.AgentHttp_HSR_TIMEOUT:
		return CompletionTimeout
	case livekit.AgentHttp_HSR_PROTOCOL:
		return CompletionProtocol
	default:
		return CompletionUnknown
	}
}

// CompletionFromTrailers reports the outcome a peer announced, or nil when it
// announced none. No announcement, the body framing having reached its end, is
// success.
func CompletionFromTrailers(t http.Header) *CompletionError {
	if len(t) == 0 {
		return nil
	}
	c := Completion(strings.ToLower(strings.TrimSpace(t.Get(completionHeader))))
	if c == CompletionUnknown {
		return nil
	}
	return &CompletionError{Completion: c, Reason: t.Get(errorHeader)}
}

// WritePreamble writes the stream preamble. Everything after it is opaque
// HTTP/1.1.
func WritePreamble(w io.Writer, p *livekit.AgentHttp_StreamPreamble) error {
	return writeLenPrefixed(w, p)
}

func ReadPreamble(r io.Reader) (*livekit.AgentHttp_StreamPreamble, error) {
	p := &livekit.AgentHttp_StreamPreamble{}
	if err := readLenPrefixed(r, p, MaxPreambleSize); err != nil {
		return nil, err
	}
	return p, nil
}

// StripReservedHeaders removes the in-band signalling fields. Every header set
// crossing between client and worker must pass through here: a client-set
// x-lk-completion would otherwise forge an outcome, and a worker-set one would
// reach the end client.
func StripReservedHeaders(h http.Header) {
	for k := range h {
		if strings.HasPrefix(strings.ToLower(k), ReservedHeaderPrefix) {
			delete(h, k)
		}
	}
}

// BuildRequestHead serializes the canonical origin-form request head.
//
// Its inputs must come from an already-parsed request, never from the client's
// own bytes: emitting a head this side has parsed is what keeps request
// smuggling out of the worker.
//
// contentLength >= 0 is framed with Content-Length; a negative length is framed
// chunked, the only framing that can carry completion trailers.
func BuildRequestHead(method, target, host string, h http.Header, contentLength int64) ([]byte, error) {
	if !httpguts.ValidHeaderFieldName(method) {
		return nil, fmt.Errorf("endpoint: invalid method %q", method)
	}
	if !strings.HasPrefix(target, "/") {
		return nil, fmt.Errorf("endpoint: request target must be origin-form, got %q", target)
	}
	if strings.ContainsAny(target, " \r\n") {
		return nil, fmt.Errorf("endpoint: invalid request target %q", target)
	}
	if !httpguts.ValidHostHeader(host) {
		return nil, fmt.Errorf("endpoint: invalid host %q", host)
	}

	var b strings.Builder
	b.WriteString(method)
	b.WriteByte(' ')
	b.WriteString(target)
	b.WriteString(" HTTP/1.1\r\nHost: ")
	b.WriteString(host)
	b.WriteString("\r\n")

	// the only framing header on the request; isFramingHeader drops any the
	// caller supplied
	if contentLength >= 0 {
		b.WriteString("Content-Length: ")
		b.WriteString(strconv.FormatInt(contentLength, 10))
		b.WriteString("\r\n")
	} else {
		b.WriteString("Transfer-Encoding: chunked\r\n")
	}

	keys := make([]string, 0, len(h))
	for k := range h {
		if isFramingHeader(k) {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic across attempts and across nodes
	for _, k := range keys {
		if !httpguts.ValidHeaderFieldName(k) {
			return nil, fmt.Errorf("endpoint: invalid header name %q", k)
		}
		for _, v := range h[k] {
			if !httpguts.ValidHeaderFieldValue(v) {
				return nil, fmt.Errorf("endpoint: invalid value for header %q", k)
			}
			b.WriteString(k)
			b.WriteString(": ")
			b.WriteString(v)
			b.WriteString("\r\n")
		}
	}
	b.WriteString("\r\n")
	return []byte(b.String()), nil
}

// isFramingHeader reports the fields whose presence would let the head declare a
// second, conflicting body framing.
func isFramingHeader(k string) bool {
	switch http.CanonicalHeaderKey(k) {
	case "Host", "Content-Length", "Transfer-Encoding", "Trailer":
		return true
	}
	return false
}

// BodyWriter frames one body onto a stream. Close ends the body; a completion
// other than success is reported in trailers where the framing allows it.
type BodyWriter interface {
	io.Writer
	Close(c Completion, reason string) error
}

// NewChunkedBody frames a body with chunked transfer coding, the only framing
// that can carry a completion trailer.
func NewChunkedBody(w io.Writer) BodyWriter { return &chunkedBody{w: w} }

// NewIdentityBody writes a body whose length the head already declared. It
// cannot carry trailers: a short body is surfaced by the FIN arriving before
// the declared length is met.
func NewIdentityBody(w io.Writer) BodyWriter { return &identityBody{w: w} }

type identityBody struct{ w io.Writer }

func (b *identityBody) Write(p []byte) (int, error)    { return b.w.Write(p) }
func (b *identityBody) Close(Completion, string) error { return nil }

type chunkedBody struct {
	w   io.Writer
	buf []byte
}

func (b *chunkedBody) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil // a zero-length chunk would terminate the body
	}
	b.buf = strconv.AppendUint(b.buf[:0], uint64(len(p)), 16)
	b.buf = append(b.buf, '\r', '\n')
	if _, err := b.w.Write(b.buf); err != nil {
		return 0, err
	}
	if _, err := b.w.Write(p); err != nil {
		return 0, err
	}
	if _, err := b.w.Write([]byte("\r\n")); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Close writes the terminal 0-chunk and, for a non-success outcome, the
// completion trailers. Success is the absence of them.
func (b *chunkedBody) Close(c Completion, reason string) error {
	var sb strings.Builder
	sb.WriteString("0\r\n")
	if c != CompletionUnknown {
		sb.WriteString(completionHeader)
		sb.WriteString(": ")
		sb.WriteString(string(c))
		sb.WriteString("\r\n")
		if reason != "" {
			sb.WriteString(errorHeader)
			sb.WriteString(": ")
			sb.WriteString(sanitizeReason(reason))
			sb.WriteString("\r\n")
		}
	}
	sb.WriteString("\r\n")
	_, err := b.w.Write([]byte(sb.String()))
	return err
}

// sanitizeReason bounds an operator-supplied string and strips the control
// characters that would break trailer framing.
func sanitizeReason(s string) string {
	const max = 256
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	if len(s) > max {
		s = s[:max]
	}
	return s
}

// CopyBody pumps src into dst one write per read, so a streaming body stays
// incremental. srcErr and dstErr are separate because only a source failure
// leaves a stream to report the outcome on.
func CopyBody(dst BodyWriter, src io.Reader, buf []byte) (n int64, srcErr, dstErr error) {
	for {
		nr, rerr := src.Read(buf)
		if nr > 0 {
			if _, werr := dst.Write(buf[:nr]); werr != nil {
				return n, nil, werr
			}
			n += int64(nr)
		}
		if rerr == io.EOF {
			return n, nil, nil
		}
		if rerr != nil {
			return n, rerr, nil
		}
	}
}

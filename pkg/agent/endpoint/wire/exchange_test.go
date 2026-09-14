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
	"bufio"
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
)

func TestPreambleRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	in := &livekit.AgentHttp_StreamPreamble{
		Kind:          livekit.AgentHttp_AEK_HTTP,
		RequestId:     "AER_abc",
		Authenticated: true,
		Route:         "/items/{id}",
		TimeoutMs:     1500,
		ClientAddr:    "203.0.113.7",
		Scheme:        "https",
	}
	require.NoError(t, WritePreamble(&buf, in))

	// the exchange bytes follow immediately; the reader must stop at the boundary
	buf.WriteString("GET / HTTP/1.1\r\n")

	out, err := ReadPreamble(&buf)
	require.NoError(t, err)
	require.Equal(t, in.RequestId, out.RequestId)
	require.True(t, out.Authenticated)
	require.Equal(t, "/items/{id}", out.Route)
	require.EqualValues(t, 1500, out.TimeoutMs)
	require.Equal(t, "https", out.Scheme)
	require.Equal(t, "GET / HTTP/1.1\r\n", buf.String(), "the preamble must not over-read")
}

func TestPreambleRejectsOversizedLength(t *testing.T) {
	var buf bytes.Buffer
	buf.Write([]byte{0xff, 0xff, 0xff, 0xff})
	_, err := ReadPreamble(&buf)
	require.Error(t, err, "a malformed length must not make the reader allocate")
}

// The head is what keeps request smuggling out of the worker, so it declares
// exactly one framing and nothing the caller supplied can add a second.
func TestBuildRequestHeadEmitsExactlyOneFraming(t *testing.T) {
	h := http.Header{}
	h.Set("Content-Length", "999")
	h.Set("Transfer-Encoding", "chunked")
	h.Set("Trailer", "x-whatever")
	h.Set("Host", "evil.example")
	h.Set("Accept", "text/plain")

	b, err := BuildRequestHead("POST", "/items/42?q=1", "good.example", h, 7)
	require.NoError(t, err)
	head := string(b)

	require.True(t, strings.HasPrefix(head, "POST /items/42?q=1 HTTP/1.1\r\n"))
	require.Equal(t, 1, strings.Count(head, "Content-Length: "))
	require.Contains(t, head, "Content-Length: 7\r\n")
	require.NotContains(t, head, "chunked")
	require.NotContains(t, head, "evil.example")
	require.NotContains(t, head, "Trailer:")
	require.Contains(t, head, "Host: good.example\r\n")
	require.Contains(t, head, "Accept: text/plain\r\n")
	require.True(t, strings.HasSuffix(head, "\r\n\r\n"))

	// net/http parses back what we emitted
	req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(head + "1234567")))
	require.NoError(t, err)
	require.Equal(t, "POST", req.Method)
	require.Equal(t, "/items/42", req.URL.Path)
	require.Equal(t, "q=1", req.URL.RawQuery)
	require.Equal(t, "good.example", req.Host)
	require.EqualValues(t, 7, req.ContentLength)
}

func TestBuildRequestHeadChunkedWhenLengthUnknown(t *testing.T) {
	b, err := BuildRequestHead("POST", "/x", "h", http.Header{}, -1)
	require.NoError(t, err)
	require.Contains(t, string(b), "Transfer-Encoding: chunked\r\n")
	require.NotContains(t, string(b), "Content-Length:")
}

func TestBuildRequestHeadRejectsInjection(t *testing.T) {
	bad := http.Header{}
	bad.Set("X-Evil", "a\r\nX-Injected: yes")
	_, err := BuildRequestHead("GET", "/x", "h", bad, 0)
	require.Error(t, err, "a header value carrying CRLF must never be serialized")

	_, err = BuildRequestHead("GET", "/x y", "h", http.Header{}, 0)
	require.Error(t, err, "a target with a space would split the request line")

	_, err = BuildRequestHead("GET", "x", "h", http.Header{}, 0)
	require.Error(t, err, "the target must be origin-form")

	_, err = BuildRequestHead("GET", "/x", "bad host\r\n", http.Header{}, 0)
	require.Error(t, err)
}

// Go parses what we emit, including trailers never announced in Trailer:.
func TestChunkedBodyAndTrailersParseWithNetHTTP(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n")
	bw := NewChunkedBody(&buf)
	_, err := bw.Write([]byte("hello"))
	require.NoError(t, err)
	require.NoError(t, bw.Close(CompletionInternal, "boom at chunk 12"))

	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(buf.Bytes())), nil)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "hello", string(body))

	ce := CompletionFromTrailers(resp.Trailer)
	require.NotNil(t, ce, "an undeclared trailer must still be read")
	require.Equal(t, CompletionInternal, ce.Completion)
	require.Equal(t, "boom at chunk 12", ce.Reason)
}

// Success is the absence of a completion trailer, never a value you send.
func TestCleanChunkedBodyReportsNoCompletion(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n")
	bw := NewChunkedBody(&buf)
	_, _ = bw.Write([]byte("hello"))
	require.NoError(t, bw.Close(CompletionUnknown, ""))

	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(buf.Bytes())), nil)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "hello", string(body))
	require.Nil(t, CompletionFromTrailers(resp.Trailer))
}

// The same bytes cut before the terminator must not read as a clean body.
func TestTruncatedChunkedBodyIsDistinguishable(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n")
	bw := NewChunkedBody(&buf)
	_, _ = bw.Write([]byte("hello"))
	// no Close: the terminal 0-chunk never arrives

	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(buf.Bytes())), nil)
	require.NoError(t, err)
	_, err = io.ReadAll(resp.Body)
	require.Error(t, err, "a body with no terminator must not read clean")
}

func TestIdentityBodyWritesNothingExtra(t *testing.T) {
	var buf bytes.Buffer
	bw := NewIdentityBody(&buf)
	_, err := bw.Write([]byte("hello"))
	require.NoError(t, err)
	// an identity body cannot carry trailers; Close must not invent framing
	require.NoError(t, bw.Close(CompletionTruncated, "ignored"))
	require.Equal(t, "hello", buf.String())
}

func TestResetCodeRoundTrip(t *testing.T) {
	for _, c := range []Completion{
		CompletionRefused, CompletionInternal, CompletionTimeout, CompletionProtocol,
	} {
		require.Equal(t, c, CompletionFromResetCode(c.ResetCode()), string(c))
	}
	// outcomes that can only arise mid-body have no code of their own
	require.Equal(t, livekit.AgentHttp_HSR_ABORT, CompletionTruncated.ResetCode())
	require.Equal(t, livekit.AgentHttp_HSR_ABORT, CompletionPeerGone.ResetCode())
	// and a bare abort must not decode as a claim that nothing was applied
	require.Equal(t, CompletionUnknown, CompletionFromResetCode(livekit.AgentHttp_HSR_ABORT))
	require.NotEqual(t, CompletionRefused, CompletionFromResetCode(livekit.AgentHttp_HSR_ABORT))
}

func TestStripReservedHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("X-Lk-Completion", "internal")
	h.Set("x-lk-error", "forged")
	h.Set("X-LK-Anything", "nope")
	h.Set("X-Keep", "yes")
	StripReservedHeaders(h)
	require.Empty(t, h.Get("X-Lk-Completion"))
	require.Empty(t, h.Get("X-Lk-Error"))
	require.Empty(t, h.Get("X-Lk-Anything"))
	require.Equal(t, "yes", h.Get("X-Keep"))
}

// A reason describing worker internals must not be able to break framing.
func TestCompletionReasonCannotBreakFraming(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n")
	bw := NewChunkedBody(&buf)
	require.NoError(t, bw.Close(CompletionInternal, "a\r\nx-injected: yes\r\n"))

	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(buf.Bytes())), nil)
	require.NoError(t, err)
	_, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Empty(t, resp.Trailer.Get("X-Injected"))
}

// oneByteReader forces reassembly across read boundaries.
type oneByteReader struct{ r io.Reader }

func (o oneByteReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return o.r.Read(p[:1])
}

func TestCopyBodySplitReads(t *testing.T) {
	src := bytes.Repeat([]byte("abcdefgh"), 1024)
	var buf bytes.Buffer
	bw := NewIdentityBody(&buf)
	n, srcErr, dstErr := CopyBody(bw, oneByteReader{bytes.NewReader(src)}, make([]byte, 512))
	require.NoError(t, srcErr)
	require.NoError(t, dstErr)
	require.EqualValues(t, len(src), n)
	require.Equal(t, src, buf.Bytes())
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

// A stream failure and a source failure are different outcomes: one can still be
// reported to the peer, the other cannot.
func TestCopyBodySeparatesSourceAndSinkFailures(t *testing.T) {
	_, srcErr, dstErr := CopyBody(NewIdentityBody(failingWriter{}), strings.NewReader("xxxx"), make([]byte, 2))
	require.NoError(t, srcErr)
	require.Error(t, dstErr)

	_, srcErr, dstErr = CopyBody(NewIdentityBody(io.Discard), iotestErrReader{}, make([]byte, 2))
	require.Error(t, srcErr)
	require.NoError(t, dstErr)
}

type iotestErrReader struct{}

func (iotestErrReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

// The body path is the hot path: chunk framing must not allocate per write.
func TestChunkedWriteDoesNotAllocatePerChunk(t *testing.T) {
	bw := NewChunkedBody(io.Discard)
	p := make([]byte, 4096)
	avg := testing.AllocsPerRun(200, func() {
		_, _ = bw.Write(p)
	})
	require.LessOrEqual(t, avg, 1.0, "chunk framing should reuse its scratch buffer")
}

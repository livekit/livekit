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

package endpoint_test

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
)

// filler produces an endless recognizable stream. The pattern is keyed to the
// absolute offset so it stays continuous across reads.
type filler struct{ off int }

func (f *filler) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte('a' + f.off%26)
		f.off++
	}
	return len(p), nil
}

// verify checks the pattern without retaining the body.
type verify struct {
	n   int64
	off int
}

func (v *verify) Write(p []byte) (int, error) {
	for _, b := range p {
		if b != byte('a'+v.off%26) {
			return 0, fmt.Errorf("corrupt byte at offset %d", v.n)
		}
		v.off++
		v.n++
	}
	return len(p), nil
}

func heapInUse() uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapInuse
}

// A response far larger than any buffer in the path streams to the client
// without the node's heap growing with the body.
func TestLargeResponseStreamsInConstantMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("moves 512MiB")
	}
	const bodySize = 512 << 20

	addr := rawTarget(t, func(c net.Conn) {
		_, _ = fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n", bodySize)
		_, _ = io.CopyN(c, &filler{}, bodySize)
	})

	base := startFramedWorker(t, addr, []*livekit.AgentHttp_AgentEndpoint{
		{Path: "/big", Methods: []string{"GET"}, Public: true},
	})

	before := heapInUse()

	resp, err := http.Get(base + "/big")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	v := &verify{}
	n, err := io.Copy(v, resp.Body)
	require.NoError(t, err)
	require.EqualValues(t, bodySize, n)

	growth := int64(heapInUse()) - int64(before)
	t.Logf("streamed %d MiB; node heap growth %d KiB", bodySize>>20, growth>>10)
	require.Less(t, growth, int64(32<<20),
		"the body must not be buffered: heap grew %d MiB streaming %d MiB", growth>>20, bodySize>>20)
}

// The same in the request direction.
func TestLargeRequestStreamsInConstantMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("moves 512MiB")
	}
	const bodySize = 512 << 20

	received := make(chan int64, 1)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		req, err := http.ReadRequest(newReqReader(c))
		if err != nil {
			return
		}
		v := &verify{}
		n, _ := io.Copy(v, req.Body)
		_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
		received <- n
	}()

	base := startFramedWorker(t, ln.Addr().String(), []*livekit.AgentHttp_AgentEndpoint{
		{Path: "/upload", Methods: []string{"POST"}, Public: true},
	})

	before := heapInUse()

	req, err := http.NewRequest(http.MethodPost, base+"/upload", io.LimitReader(&filler{}, bodySize))
	require.NoError(t, err)
	req.ContentLength = bodySize
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	require.EqualValues(t, bodySize, <-received)

	growth := int64(heapInUse()) - int64(before)
	t.Logf("uploaded %d MiB; node heap growth %d KiB", bodySize>>20, growth>>10)
	require.Less(t, growth, int64(32<<20),
		"the body must not be buffered: heap grew %d MiB uploading %d MiB", growth>>20, bodySize>>20)
}

func newReqReader(c net.Conn) *bufio.Reader { return bufio.NewReader(c) }

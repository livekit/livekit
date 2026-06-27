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

// Command test-server is a programmable mock of the LiveKit server HTTP API,
// used by the server SDKs to test client behavior. See cmd/test-server/README.md.
package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/livekit/protocol/livekit"
)

// X-Lk-Mock-* request headers control the mock's behavior; see the README.
const (
	headerFailRegions   = "X-Lk-Mock-Fail-Regions"
	headerFailMode      = "X-Lk-Mock-Fail-Mode"
	headerFailStatus    = "X-Lk-Mock-Fail-Status"
	headerFailTwirpCode = "X-Lk-Mock-Fail-Twirp-Code"
	headerDelayMs       = "X-Lk-Mock-Delay-Ms"
	headerRegionsStatus = "X-Lk-Mock-Regions-Status"
	headerResponse      = "X-Lk-Mock-Response"
	// headerRegion is set on responses to the index of the region that served it.
	headerRegion = "X-Lk-Mock-Region"
)

const defaultDelayMs = 30_000

func main() {
	portsFlag := flagValue("--ports", "LK_TEST_SERVER_PORTS", "9999,10000,10001,10002")
	advertiseHost := flagValue("--advertise-host", "LK_TEST_SERVER_ADVERTISE_HOST", "http://127.0.0.1")
	bindAddr := flagValue("--bind", "LK_TEST_SERVER_BIND", "0.0.0.0")
	twirpPrefix := flagValue("--twirp-prefix", "LK_TEST_SERVER_TWIRP_PREFIX", "/twirp")

	ports, err := parsePorts(portsFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid --ports: %v\n", err)
		os.Exit(1)
	}
	advertiseHost = strings.TrimRight(advertiseHost, "/")

	regions := &livekit.RegionSettings{}
	for i, p := range ports {
		regions.Regions = append(regions.Regions, &livekit.RegionInfo{
			Region:   fmt.Sprintf("region-%d", i),
			Url:      fmt.Sprintf("%s:%d", advertiseHost, p),
			Distance: int64(i),
		})
	}

	errCh := make(chan error, len(ports))
	for i, p := range ports {
		srv := &http.Server{
			Addr:    fmt.Sprintf("%s:%d", bindAddr, p),
			Handler: &mockHandler{regionIndex: i, regions: regions, twirpPrefix: twirpPrefix},
		}
		go func() { errCh <- srv.ListenAndServe() }()
		fmt.Printf("test-server: region-%d listening on %s:%d (advertised as %s:%d)\n", i, bindAddr, p, advertiseHost, p)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-errCh:
		fmt.Fprintf(os.Stderr, "listener failed: %v\n", err)
		os.Exit(1)
	case <-sigCh:
		fmt.Println("test-server: shutting down")
	}
}

type mockHandler struct {
	regionIndex int
	regions     *livekit.RegionSettings
	twirpPrefix string
}

func (h *mockHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/settings/regions":
		h.handleRegions(w, r)
	case strings.HasPrefix(r.URL.Path, h.twirpPrefix+"/"):
		h.handleTwirp(w, r)
	case r.URL.Path == "/" || r.URL.Path == "/_test/health":
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	default:
		http.NotFound(w, r)
	}
}

func (h *mockHandler) handleRegions(w http.ResponseWriter, r *http.Request) {
	if status := parseStatus(r.Header.Get(headerRegionsStatus), 0); status != 0 && status != http.StatusOK {
		w.WriteHeader(status)
		return
	}
	body, err := protojson.Marshal(h.regions)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "max-age=0")
	w.Header().Set(headerRegion, strconv.Itoa(h.regionIndex))
	_, _ = w.Write(body)
}

func (h *mockHandler) handleTwirp(w http.ResponseWriter, r *http.Request) {
	if h.shouldFail(r) {
		h.fail(w, r)
		return
	}
	h.writeAPIResponse(w, r)
}

func (h *mockHandler) shouldFail(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get(headerFailRegions), ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if idx, err := strconv.Atoi(part); err == nil && idx == h.regionIndex {
			return true
		}
	}
	return false
}

func (h *mockHandler) fail(w http.ResponseWriter, r *http.Request) {
	switch strings.ToLower(r.Header.Get(headerFailMode)) {
	case "drop":
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
				return
			}
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	case "delay":
		delay := defaultDelayMs
		if ms, err := strconv.Atoi(r.Header.Get(headerDelayMs)); err == nil && ms >= 0 {
			delay = ms
		}
		time.Sleep(time.Duration(delay) * time.Millisecond)
		writeTwirpError(w, r, parseStatus(r.Header.Get(headerFailStatus), http.StatusServiceUnavailable))
	default:
		writeTwirpError(w, r, parseStatus(r.Header.Get(headerFailStatus), http.StatusServiceUnavailable))
	}
}

func writeTwirpError(w http.ResponseWriter, r *http.Request, status int) {
	code := r.Header.Get(headerFailTwirpCode)
	if code == "" {
		code = twirpCodeForStatus(status)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set(headerRegion, "")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"code":%q,"msg":%q}`, code, fmt.Sprintf("mock failure (status %d)", status))
}

func twirpCodeForStatus(status int) string {
	switch {
	case status == http.StatusBadRequest:
		return "invalid_argument"
	case status == http.StatusUnauthorized:
		return "unauthenticated"
	case status == http.StatusForbidden:
		return "permission_denied"
	case status == http.StatusNotFound:
		return "not_found"
	case status == http.StatusTooManyRequests:
		return "resource_exhausted"
	case status >= 500:
		return "unavailable"
	default:
		return "internal"
	}
}

func parseStatus(s string, def int) int {
	if s == "" {
		return def
	}
	if v, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && v >= 100 && v <= 599 {
		return v
	}
	return def
}

func parsePorts(s string) ([]int, error) {
	var ports []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		v, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("%q is not a port number", part)
		}
		ports = append(ports, v)
	}
	if len(ports) == 0 {
		return nil, errors.New("at least one port is required")
	}
	return ports, nil
}

// flagValue resolves a setting from a --flag, then an environment variable, then a default.
func flagValue(flag, env, def string) string {
	prefix := flag + "="
	for i, arg := range os.Args[1:] {
		if arg == flag {
			if i+2 <= len(os.Args[1:]) {
				return os.Args[1:][i+1]
			}
		}
		if strings.HasPrefix(arg, prefix) {
			return strings.TrimPrefix(arg, prefix)
		}
	}
	if v := os.Getenv(env); v != "" {
		return v
	}
	return def
}

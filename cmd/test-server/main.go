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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/twitchtv/twirp"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/utils/protojson"
)

func main() {
	portsFlag := flagValue("--ports", "LK_TEST_SERVER_PORTS", "9999,10000,10001,10002")
	advertiseHost := flagValue("--advertise-host", "LK_TEST_SERVER_ADVERTISE_HOST", "http://127.0.0.1")
	bindAddr := flagValue("--bind", "LK_TEST_SERVER_BIND", "0.0.0.0")
	twirpPrefix := flagValue("--twirp-prefix", "LK_TEST_SERVER_TWIRP_PREFIX", "/twirp")
	// API secret used to verify request tokens for permission enforcement.
	// Defaults to the `livekit-server --dev` secret.
	apiSecret := flagValue("--api-secret", "LK_TEST_SERVER_API_SECRET", "secret")

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
			Handler: &mockHandler{regionIndex: i, regions: regions, twirpPrefix: twirpPrefix, apiSecret: apiSecret},
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
	apiSecret   string
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
	cfg := parseMockConfig(r)
	if cfg.RegionsStatus != 0 && cfg.RegionsStatus != http.StatusOK {
		w.WriteHeader(cfg.RegionsStatus)
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
	h.serveAPI(w, r)
}

func (h *mockHandler) shouldFail(cfg *mockConfig) bool {
	return slices.Contains(cfg.FailRegions, h.regionIndex)
}

func (h *mockHandler) fail(w http.ResponseWriter, cfg *mockConfig) {
	switch strings.ToLower(cfg.FailMode) {
	case "drop":
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
				return
			}
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	case "delay":
		// Deprecated legacy mode: sleep, then status-fail. New clients should set
		// DelayMs (which delays every response) instead.
		time.Sleep(time.Duration(cfg.legacyDelayMs) * time.Millisecond)
	}
	status := cfg.FailStatus
	if status < 100 || status > 599 {
		status = http.StatusServiceUnavailable
	}
	writeTwirpError(w, cfg, status)
}

func writeTwirpError(w http.ResponseWriter, cfg *mockConfig, status int) {
	code := cfg.FailTwirpCode
	if code == "" {
		code = twirpCodeForStatus(status)
	}
	writeTwirpErrorCode(w, status, code, fmt.Sprintf("mock failure (status %d)", status))
}

// writeTwirpErrorCode writes a Twirp JSON error with an explicit code and message.
func writeTwirpErrorCode(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set(headerRegion, "")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"code":%q,"msg":%q}`, code, msg)
}

// writeTwirpErr writes a full Twirp JSON error — code, message, and metadata —
// using the HTTP status Twirp derives from the error code.
func writeTwirpErr(w http.ResponseWriter, terr twirp.Error) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set(headerRegion, "")
	w.WriteHeader(twirp.ServerHTTPStatusFromErrorCode(terr.Code()))
	_ = json.NewEncoder(w).Encode(struct {
		Code string            `json:"code"`
		Msg  string            `json:"msg"`
		Meta map[string]string `json:"meta,omitempty"`
	}{
		Code: string(terr.Code()),
		Msg:  terr.Msg(),
		Meta: terr.MetaMap(),
	})
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

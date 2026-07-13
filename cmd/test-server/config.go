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

package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

const (
	// headerMock carries the whole mock control config as a JSON object; see
	// mockConfig and the README. Sent by the SDK on API calls (and forwarded onto
	// the /settings/regions fetch and every failover retry).
	headerMock = "X-Lk-Mock"
	// headerRegion is set on responses to the index of the region that served it.
	headerRegion = "X-Lk-Mock-Region"
)

// Deprecated: the individual X-Lk-Mock-* control headers predate the unified
// X-Lk-Mock JSON header. They are still honored for existing clients; new
// clients should send X-Lk-Mock instead. When X-Lk-Mock is present its fields
// take precedence over any legacy header.
const (
	legacyHeaderFailRegions   = "X-Lk-Mock-Fail-Regions"
	legacyHeaderFailMode      = "X-Lk-Mock-Fail-Mode"
	legacyHeaderFailStatus    = "X-Lk-Mock-Fail-Status"
	legacyHeaderFailTwirpCode = "X-Lk-Mock-Fail-Twirp-Code"
	legacyHeaderDelayMs       = "X-Lk-Mock-Delay-Ms"
	legacyHeaderRegionsStatus = "X-Lk-Mock-Regions-Status"
	legacyHeaderResponse      = "X-Lk-Mock-Response"
	legacyHeaderSkipAuth      = "X-Lk-Mock-Skip-Auth"
)

// legacyDefaultDelayMs is the sleep used by the deprecated "delay" fail mode when
// no X-Lk-Mock-Delay-Ms is given (long enough to trip client timeouts).
const legacyDefaultDelayMs = 30_000

// mockConfig is the JSON value of the X-Lk-Mock request header. Every field is
// optional; the zero value means "behave normally". A single object keeps the
// control protocol simple — the SDK serializes one struct instead of juggling a
// header per knob.
type mockConfig struct {
	// FailRegions lists region indices that should fail this request. A listener
	// fails only if its own region index appears here, so one config can make the
	// primary fail while a fallback succeeds.
	FailRegions []int `json:"failRegions,omitempty"`
	// FailMode selects how a failing region fails: "status" (default) writes a
	// Twirp error; "drop" closes the connection to force a transport error.
	// ("delay" is a deprecated legacy mode; new clients use DelayMs instead.)
	FailMode string `json:"failMode,omitempty"`
	// FailStatus is the HTTP status for a "status"-mode failure (default 503).
	FailStatus int `json:"failStatus,omitempty"`
	// FailTwirpCode overrides the Twirp error code string in the failure body
	// (default derived from FailStatus).
	FailTwirpCode string `json:"failTwirpCode,omitempty"`
	// DelayMs delays the response by this many milliseconds before returning,
	// whether the region succeeds or fails. It overrides a method's natural
	// latency (see methodLatency): set it high for timeout tests, or to 0 to skip
	// a SIP method's built-in wait. Nil means "use the natural latency".
	DelayMs *int `json:"delayMs,omitempty"`
	// RegionsStatus overrides the HTTP status of GET /settings/regions (default 200).
	RegionsStatus int `json:"regionsStatus,omitempty"`
	// Response is the protojson of the response message for the called method; it
	// replaces the populated default, giving full control over the payload.
	Response json.RawMessage `json:"response,omitempty"`
	// SkipAuth disables permission enforcement for this request (for tests that
	// aren't about authz, e.g. failover tests with a placeholder token).
	SkipAuth bool `json:"skipAuth,omitempty"`
	// SIPStatus, when set on a SIP dial method (CreateSIPParticipant /
	// TransferSIPParticipant), fails the call with this SIP status. The Twirp
	// error code and metadata (sip_status_code, sip_status, error_details) are
	// derived from it exactly as the real server does, so the SDK sees an
	// identical error. Composes with DelayMs to simulate "ring, then fail".
	SIPStatus *sipStatusConfig `json:"sipStatus,omitempty"`

	// legacyDelayMs is the sleep used by the deprecated "delay" fail mode. It is
	// populated only from the legacy X-Lk-Mock-Delay-Ms header, never from JSON.
	legacyDelayMs int
}

// sipStatusConfig is a SIP response to inject; see mockConfig.SIPStatus.
type sipStatusConfig struct {
	// Code is the SIP response code, e.g. 486 (Busy Here) or 603 (Decline).
	Code int `json:"code"`
	// Status is the SIP reason phrase; defaults to the code's canonical name.
	Status string `json:"status,omitempty"`
}

// parseMockConfig builds the request's config. Deprecated individual X-Lk-Mock-*
// headers form the base; the unified X-Lk-Mock JSON header (if present) is
// overlaid on top, so its fields win per-field while absent fields keep the
// legacy value.
func parseMockConfig(r *http.Request) mockConfig {
	cfg := parseLegacyConfig(r)
	if v := r.Header.Get(headerMock); v != "" {
		// Unmarshal overwrites only the fields present in the JSON; the unexported
		// legacyDelayMs is untouched.
		_ = json.Unmarshal([]byte(v), &cfg)
	}
	return cfg
}

// parseLegacyConfig reads the deprecated per-setting headers into a config.
func parseLegacyConfig(r *http.Request) mockConfig {
	var cfg mockConfig
	for _, part := range strings.Split(r.Header.Get(legacyHeaderFailRegions), ",") {
		if idx, err := strconv.Atoi(strings.TrimSpace(part)); err == nil {
			cfg.FailRegions = append(cfg.FailRegions, idx)
		}
	}
	cfg.FailMode = r.Header.Get(legacyHeaderFailMode)
	cfg.FailStatus = parseStatus(r.Header.Get(legacyHeaderFailStatus))
	cfg.FailTwirpCode = r.Header.Get(legacyHeaderFailTwirpCode)
	cfg.RegionsStatus = parseStatus(r.Header.Get(legacyHeaderRegionsStatus))
	if resp := r.Header.Get(legacyHeaderResponse); resp != "" {
		cfg.Response = json.RawMessage(resp)
	}
	cfg.SkipAuth = strings.EqualFold(r.Header.Get(legacyHeaderSkipAuth), "true")
	cfg.legacyDelayMs = legacyDefaultDelayMs
	if ms, err := strconv.Atoi(r.Header.Get(legacyHeaderDelayMs)); err == nil && ms >= 0 {
		cfg.legacyDelayMs = ms
	}
	return cfg
}

// parseStatus returns a valid HTTP status from s, or 0 if absent/invalid.
func parseStatus(s string) int {
	if v, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && v >= 100 && v <= 599 {
		return v
	}
	return 0
}

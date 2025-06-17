// Copyright 2023 LiveKit, Inc.
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

package service

import (
	"context"
	"errors"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/protocol/livekit"
	"github.com/ua-parser/uap-go/uaparser"
)

func HandleError(w http.ResponseWriter, r *http.Request, status int, err error, keysAndValues ...interface{}) {
	keysAndValues = append(keysAndValues, "status", status)
	if r != nil && r.URL != nil {
		keysAndValues = append(keysAndValues, "method", r.Method, "path", r.URL.Path)
	}
	if !errors.Is(err, context.Canceled) && !errors.Is(r.Context().Err(), context.Canceled) {
		utils.GetLogger(r.Context()).WithCallDepth(1).Warnw("error handling request", err, keysAndValues...)
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(err.Error()))
}

func boolValue(s string) bool {
	return s == "1" || s == "true"
}

func RemoveDoubleSlashes(w http.ResponseWriter, r *http.Request, next http.HandlerFunc) {
	if strings.HasPrefix(r.URL.Path, "//") {
		r.URL.Path = r.URL.Path[1:]
	}
	next(w, r)
}

func IsValidDomain(domain string) bool {
	domainRegexp := regexp.MustCompile(`^(?i)[a-z0-9-]+(\.[a-z0-9-]+)+\.?$`)
	return domainRegexp.MatchString(domain)
}

func GetClientIP(r *http.Request) string {
	// CF proxy typically is first thing the user reaches
	if ip := r.Header.Get("CF-Connecting-IP"); ip != "" {
		return ip
	}
	if ip := r.Header.Get("X-Forwarded-For"); ip != "" {
		return ip
	}
	if ip := r.Header.Get("X-Real-IP"); ip != "" {
		return ip
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	return ip
}

func SetRoomConfiguration(createRequest *livekit.CreateRoomRequest, conf *livekit.RoomConfiguration) {
	if conf == nil {
		return
	}
	createRequest.Agents = conf.Agents
	createRequest.Egress = conf.Egress
	createRequest.EmptyTimeout = conf.EmptyTimeout
	createRequest.DepartureTimeout = conf.DepartureTimeout
	createRequest.MaxParticipants = conf.MaxParticipants
	createRequest.MinPlayoutDelay = conf.MinPlayoutDelay
	createRequest.MaxPlayoutDelay = conf.MaxPlayoutDelay
	createRequest.SyncStreams = conf.SyncStreams
}

func ParseClientInfo(r *http.Request) *livekit.ClientInfo {
	values := r.Form
	ci := &livekit.ClientInfo{}
	if pv, err := strconv.Atoi(values.Get("protocol")); err == nil {
		ci.Protocol = int32(pv)
	}
	sdkString := values.Get("sdk")
	switch sdkString {
	case "js":
		ci.Sdk = livekit.ClientInfo_JS
	case "ios", "swift":
		ci.Sdk = livekit.ClientInfo_SWIFT
	case "android":
		ci.Sdk = livekit.ClientInfo_ANDROID
	case "flutter":
		ci.Sdk = livekit.ClientInfo_FLUTTER
	case "go":
		ci.Sdk = livekit.ClientInfo_GO
	case "unity":
		ci.Sdk = livekit.ClientInfo_UNITY
	case "reactnative":
		ci.Sdk = livekit.ClientInfo_REACT_NATIVE
	case "rust":
		ci.Sdk = livekit.ClientInfo_RUST
	case "python":
		ci.Sdk = livekit.ClientInfo_PYTHON
	case "cpp":
		ci.Sdk = livekit.ClientInfo_CPP
	case "unityweb":
		ci.Sdk = livekit.ClientInfo_UNITY_WEB
	case "node":
		ci.Sdk = livekit.ClientInfo_NODE
	}

	ci.Version = values.Get("version")
	ci.Os = values.Get("os")
	ci.OsVersion = values.Get("os_version")
	ci.Browser = values.Get("browser")
	ci.BrowserVersion = values.Get("browser_version")
	ci.DeviceModel = values.Get("device_model")
	ci.Network = values.Get("network")
	// get real address (forwarded http header) - check Cloudflare headers first, fall back to X-Forwarded-For
	ci.Address = GetClientIP(r)

	// attempt to parse types for SDKs that support browser as a platform
	if ci.Sdk == livekit.ClientInfo_JS ||
		ci.Sdk == livekit.ClientInfo_REACT_NATIVE ||
		ci.Sdk == livekit.ClientInfo_FLUTTER ||
		ci.Sdk == livekit.ClientInfo_UNITY {
		client := uaparser.NewFromSaved().Parse(r.UserAgent())
		if ci.Browser == "" {
			ci.Browser = client.UserAgent.Family
			ci.BrowserVersion = client.UserAgent.ToVersionString()
		}
		if ci.Os == "" {
			ci.Os = client.Os.Family
			ci.OsVersion = client.Os.ToVersionString()
		}
		if ci.DeviceModel == "" {
			model := client.Device.Family
			if model != "" && client.Device.Model != "" && model != client.Device.Model {
				model += " " + client.Device.Model
			}

			ci.DeviceModel = model
		}
	}

	return ci
}

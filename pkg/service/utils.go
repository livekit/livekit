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
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/ua-parser/uap-go/uaparser"
	"gopkg.in/yaml.v3"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/routing/selector"
	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

func handleError(w http.ResponseWriter, r *http.Request, status int, err error, keysAndValues ...interface{}) {
	keysAndValues = append(keysAndValues, "status", status)
	if r != nil && r.URL != nil {
		keysAndValues = append(keysAndValues, "method", r.Method, "path", r.URL.Path)
	}
	if !errors.Is(err, context.Canceled) && !errors.Is(r.Context().Err(), context.Canceled) {
		utils.GetLogger(r.Context()).WithCallDepth(1).Warnw("error handling request", err, keysAndValues...)
	}
	w.WriteHeader(status)
}

func HandleError(w http.ResponseWriter, r *http.Request, status int, err error, keysAndValues ...interface{}) {
	handleError(w, r, status, err, keysAndValues...)
	_, _ = w.Write([]byte(err.Error()))
}

func HandleErrorJson(w http.ResponseWriter, r *http.Request, status int, err error, keysAndValues ...interface{}) {
	handleError(w, r, status, err, keysAndValues...)
	json.NewEncoder(w).Encode(struct {
		Error string `json:"error"`
	}{
		Error: err.Error(),
	})
	w.Header().Add("Content-type", "application/json")
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
	createRequest.Metadata = conf.Metadata
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

	AugmentClientInfo(ci, r)

	return ci
}

var (
	userAgentParserCache *uaparser.Parser
	userAgentParserInit  sync.Once
)

func createUserAgentParserWithCustomRules() (*uaparser.Parser, error) {
	defaultYaml := uaparser.DefinitionYaml

	rules := make(map[string]interface{})
	err := yaml.Unmarshal(defaultYaml, rules)
	if err != nil {
		return nil, err
	}

	rules["user_agent_parsers"] = append(rules["user_agent_parsers"].([]interface{}), map[string]interface{}{
		"regex":              "OBS-Studio\\/([0-9\\.]+)",
		"family_replacement": "OBS Studio",
		"v1_replacement":     "$1",
	})

	customYaml, err := yaml.Marshal(rules)
	if err != nil {
		return nil, err
	}

	return uaparser.NewFromBytes([]byte(customYaml))
}

func getUserAgentParser() *uaparser.Parser {
	userAgentParserInit.Do(func() {
		if parser, err := createUserAgentParserWithCustomRules(); err != nil {
			logger.Warnw("could not create user agent parser with custom rules, using default", err)
			userAgentParserCache = uaparser.NewFromSaved()
		} else {
			userAgentParserCache = parser
		}
	})
	return userAgentParserCache
}

func AugmentClientInfo(ci *livekit.ClientInfo, req *http.Request) {
	// get real address (forwarded http header) - check Cloudflare headers first, fall back to X-Forwarded-For
	ci.Address = GetClientIP(req)

	// attempt to parse types for SDKs that support browser as a platform
	if ci.Sdk == livekit.ClientInfo_JS ||
		ci.Sdk == livekit.ClientInfo_REACT_NATIVE ||
		ci.Sdk == livekit.ClientInfo_FLUTTER ||
		ci.Sdk == livekit.ClientInfo_UNITY ||
		ci.Sdk == livekit.ClientInfo_UNKNOWN {
		client := getUserAgentParser().Parse(req.UserAgent())
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
}

type ValidateConnectRequestParams struct {
	roomName   livekit.RoomName
	publish    string
	metadata   string
	attributes map[string]string
}

type ValidateConnectRequestResult struct {
	roomName          livekit.RoomName
	grants            *auth.ClaimGrants
	region            string
	createRoomRequest *livekit.CreateRoomRequest
}

func ValidateConnectRequest(
	lgr logger.Logger,
	r *http.Request,
	limitConfig config.LimitConfig,
	params ValidateConnectRequestParams,
	router routing.MessageRouter,
	roomAllocator RoomAllocator,
) (ValidateConnectRequestResult, int, error) {
	var res ValidateConnectRequestResult

	// require a claim
	claims := GetGrants(r.Context())
	if claims == nil || claims.Video == nil {
		return res, http.StatusUnauthorized, rtc.ErrPermissionDenied
	}

	roomNameInToken, err := EnsureJoinPermission(r.Context())
	if err != nil {
		return res, http.StatusUnauthorized, err
	}

	if claims.Identity == "" {
		return res, http.StatusBadRequest, ErrIdentityEmpty
	}
	if !limitConfig.CheckParticipantIdentityLength(claims.Identity) {
		return res, http.StatusBadRequest, fmt.Errorf("%w: max length %d", ErrParticipantIdentityExceedsLimits, limitConfig.MaxParticipantIdentityLength)
	}

	if claims.RoomConfig != nil {
		if err := claims.RoomConfig.CheckCredentials(); err != nil {
			lgr.Warnw("credentials found in token", nil)
			// TODO(dz): in a future version, we'll reject these connections
		}
	}

	res.roomName = params.roomName
	if roomNameInToken != "" {
		res.roomName = roomNameInToken
	}
	if res.roomName == "" {
		return res, http.StatusBadRequest, ErrNoRoomName
	}
	if !limitConfig.CheckRoomNameLength(string(res.roomName)) {
		return res, http.StatusBadRequest, fmt.Errorf("%w: max length %d", ErrRoomNameExceedsLimits, limitConfig.MaxRoomNameLength)
	}

	// this is new connection for existing participant -  with publish only permissions
	if params.publish != "" {
		// Make sure grant has GetCanPublish set,
		if !claims.Video.GetCanPublish() {
			return res, http.StatusUnauthorized, rtc.ErrPermissionDenied
		}
		// Make sure by default subscribe is off
		claims.Video.SetCanSubscribe(false)
		claims.Identity += "#" + params.publish
	}

	// room allocator validations
	err = roomAllocator.ValidateCreateRoom(r.Context(), res.roomName)
	if err != nil {
		if errors.Is(err, ErrRoomNotFound) {
			return res, http.StatusNotFound, err
		} else {
			return res, http.StatusInternalServerError, err
		}
	}

	if router, ok := router.(routing.Router); ok {
		res.region = router.GetRegion()
		if foundNode, err := router.GetNodeForRoom(r.Context(), res.roomName); err == nil {
			if selector.LimitsReached(limitConfig, foundNode.Stats) {
				return res, http.StatusServiceUnavailable, rtc.ErrLimitExceeded
			}
		}
	}

	createRequest := &livekit.CreateRoomRequest{
		Name:       string(res.roomName),
		RoomPreset: claims.RoomPreset,
	}
	SetRoomConfiguration(createRequest, claims.GetRoomConfiguration())
	res.createRoomRequest = createRequest

	if len(params.metadata) != 0 {
		// Make sure grant has GetCanUpdateOwnMetadata set
		if !claims.Video.GetCanUpdateOwnMetadata() {
			return res, http.StatusUnauthorized, rtc.ErrPermissionDenied
		}
		claims.Metadata = params.metadata
	}

	// Add extra attributes to the participant
	if len(params.attributes) != 0 {
		// Make sure grant has GetCanUpdateOwnMetadata set
		if !claims.Video.GetCanUpdateOwnMetadata() {
			return res, http.StatusUnauthorized, rtc.ErrPermissionDenied
		}
		if claims.Attributes == nil {
			claims.Attributes = make(map[string]string, len(params.attributes))
		}
		for k, v := range params.attributes {
			if v == "" {
				continue // do not allow deleting existing attributes
			}
			claims.Attributes[k] = v
		}
	}

	res.grants = claims
	return res, http.StatusOK, nil
}

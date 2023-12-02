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
	"net"
	"net/http"
	"regexp"

	"github.com/livekit/protocol/logger"
)

func handleError(w http.ResponseWriter, r *http.Request, status int, err error, keysAndValues ...interface{}) {
	keysAndValues = append(keysAndValues, "status", status)
	if r != nil && r.URL != nil {
		keysAndValues = append(keysAndValues, "method", r.Method, "path", r.URL.Path)
	}
	logger.GetLogger().WithCallDepth(1).Warnw("error handling request", err, keysAndValues...)
	w.WriteHeader(status)
	_, _ = w.Write([]byte(err.Error()))
}

func boolValue(s string) bool {
	return s == "1" || s == "true"
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

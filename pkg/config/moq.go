// Copyright 2026 LiveKit, Inc.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// MoQConfig controls the native QUIC data plane on this room node.
type MoQConfig struct {
	Enabled  bool   `yaml:"enabled,omitempty"`
	Endpoint string `yaml:"endpoint,omitempty"`
}

func (c MoQConfig) Validate(listenerPort uint32) error {
	if !c.Enabled {
		return nil
	}
	if listenerPort == 0 || listenerPort > 65535 {
		return errors.New("moq.enabled requires webtransport.port between 1 and 65535")
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Scheme != "moqt" || u.Hostname() == "" || u.EscapedPath() != "/moq" || u.User != nil || u.ForceQuery || u.RawQuery != "" || strings.Contains(c.Endpoint, "#") {
		return errors.New("moq.endpoint must be a moqt://host:port/moq URL without credentials, query or fragment")
	}
	host, portText, err := net.SplitHostPort(u.Host)
	if err != nil || host == "" {
		return errors.New("moq.endpoint requires a host and explicit port")
	}
	if port, err := strconv.ParseUint(portText, 10, 16); err != nil || port == 0 {
		return errors.New("moq.endpoint requires an explicit port between 1 and 65535")
	}
	return nil
}

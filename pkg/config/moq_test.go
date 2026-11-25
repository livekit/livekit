// Copyright 2026 LiveKit, Inc.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMoQConfig(t *testing.T) {
	conf, err := NewConfig("", true, nil, nil)
	require.NoError(t, err)
	require.False(t, conf.MoQ.Enabled)

	conf, err = NewConfig("moq:\n  endpoint: moqt://example.org:443/moq", true, nil, nil)
	require.NoError(t, err)
	require.False(t, conf.MoQ.Enabled)
	require.Zero(t, conf.WebTransport.Port)

	conf, err = NewConfig("moq:\n  enabled: true\n  endpoint: moqt://[::1]:443/moq\nwebtransport:\n  port: 7883", true, nil, nil)
	require.NoError(t, err)
	require.True(t, conf.MoQ.Enabled)
	require.Equal(t, "moqt://[::1]:443/moq", conf.MoQ.Endpoint)
	_, err = NewConfig("moq:\n  enabled: true", true, nil, nil)
	require.ErrorContains(t, err, "webtransport.port")

	for _, port := range []uint32{0, 65536} {
		require.Error(t, conf.MoQ.Validate(port))
	}
	for _, endpoint := range []string{
		"", "https://example.org:443/moq", "moqt://:443/moq", "moqt://example.org/moq",
		"moqt://example.org:0/moq", "moqt://example.org:65536/moq", "moqt://example.org:443/",
		"moqt://user@example.org:443/moq", "moqt://example.org:443/moq?ticket=x",
		"moqt://example.org:443/moq#credentials", "moqt://example.org:443/moq?", "moqt://example.org:443/m%6fq", "moqt://host:other:443/moq",
	} {
		require.Error(t, (MoQConfig{Enabled: true, Endpoint: endpoint}).Validate(7883), endpoint)
	}
}

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

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestH264AccessUnitAssemblerCachesParameterSets(t *testing.T) {
	a := newH264AccessUnitAssembler()

	au, err := a.Push([]byte{0x67, 0x42, 0x00, 0x1f}, 10, false)
	require.NoError(t, err)
	require.Nil(t, au)
	au, err = a.Push([]byte{0x68, 0xce, 0x06, 0xe2}, 10, false)
	require.NoError(t, err)
	require.Nil(t, au)
	au, err = a.Push([]byte{0x65, 0x88, 0x84}, 10, true)
	require.NoError(t, err)
	require.NotNil(t, au)
	require.True(t, au.HasIDR)
	require.True(t, au.HasSPS)
	require.True(t, au.HasPPS)
	require.Equal(t, appendAnnexB(
		[]byte{0x67, 0x42, 0x00, 0x1f},
		[]byte{0x68, 0xce, 0x06, 0xe2},
		[]byte{0x65, 0x88, 0x84},
	), au.Payload)
}

func TestH264AccessUnitAssemblerPrependsCachedParameterSetsToIDR(t *testing.T) {
	a := newH264AccessUnitAssembler()
	_, err := a.Push([]byte{0x67, 0x42, 0x00, 0x1f}, 10, false)
	require.NoError(t, err)
	_, err = a.Push([]byte{0x68, 0xce, 0x06, 0xe2}, 10, false)
	require.NoError(t, err)
	_, err = a.Push([]byte{0x65, 0x88, 0x84}, 10, true)
	require.NoError(t, err)

	au, err := a.Push([]byte{0x65, 0xaa, 0xbb}, 20, true)
	require.NoError(t, err)
	require.NotNil(t, au)
	require.True(t, au.HasIDR)
	require.True(t, au.HasSPS)
	require.True(t, au.HasPPS)
	require.Equal(t, appendAnnexB(
		[]byte{0x67, 0x42, 0x00, 0x1f},
		[]byte{0x68, 0xce, 0x06, 0xe2},
		[]byte{0x65, 0xaa, 0xbb},
	), au.Payload)
}

func TestH264AccessUnitAssemblerReconstructsFUA(t *testing.T) {
	a := newH264AccessUnitAssembler()

	au, err := a.Push([]byte{0x7c, 0x85, 0x01, 0x02}, 10, false)
	require.NoError(t, err)
	require.Nil(t, au)
	au, err = a.Push([]byte{0x7c, 0x05, 0x03, 0x04}, 10, false)
	require.NoError(t, err)
	require.Nil(t, au)
	au, err = a.Push([]byte{0x7c, 0x45, 0x05, 0x06}, 10, true)
	require.NoError(t, err)
	require.NotNil(t, au)
	require.True(t, au.HasIDR)
	require.Equal(t, appendAnnexB([]byte{0x65, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06}), au.Payload)
}

func appendAnnexB(nalus ...[]byte) []byte {
	var out []byte
	for _, nalu := range nalus {
		out = append(out, annexBStartCode...)
		out = append(out, nalu...)
	}
	return out
}

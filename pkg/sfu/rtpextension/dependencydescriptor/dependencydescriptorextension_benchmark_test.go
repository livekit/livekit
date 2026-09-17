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

package dependencydescriptor

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
)

// dependencyDescriptorMarshalFixture is a dependency descriptor captured from
// traffic. The fixture helper decodes its attached structure once, then returns
// a regular per-packet descriptor that references that structure.
const dependencyDescriptorMarshalFixture = "c1017280081485214eafffaaaa863cf0430c10c302afc0aaa0063c00430010c002a000a80006000040001d954926e082b04a0941b820ac1282503157f974000ca864330e222222eca8655304224230eca877530077004200ef008601df010d"

func newDependencyDescriptorMarshalFixture(tb testing.TB) *DependencyDescriptorExtension {
	tb.Helper()

	buf, err := hex.DecodeString(dependencyDescriptorMarshalFixture)
	require.NoError(tb, err)

	descriptor := DependencyDescriptor{}
	parser := DependencyDescriptorExtension{Descriptor: &descriptor}
	_, err = parser.Unmarshal(buf)
	require.NoError(tb, err)
	require.NotNil(tb, descriptor.AttachedStructure, "fixture did not contain a dependency structure")

	structure := descriptor.AttachedStructure
	descriptor.AttachedStructure = nil
	descriptor.ActiveDecodeTargetsBitmask = nil

	return &DependencyDescriptorExtension{
		Descriptor: &descriptor,
		Structure:  structure,
	}
}

func checkDependencyDescriptorMarshal(tb testing.TB, buf []byte, structure *FrameDependencyStructure, frameNumber uint16) {
	tb.Helper()

	require.NotEmpty(tb, buf, "marshal returned an empty dependency descriptor")

	decoded := DependencyDescriptor{}
	parser := DependencyDescriptorExtension{
		Descriptor: &decoded,
		Structure:  structure,
	}
	_, err := parser.Unmarshal(buf)
	require.NoError(tb, err)
	require.Equal(tb, frameNumber, decoded.FrameNumber)
}

func TestDependencyDescriptorMarshalRoundTrip(t *testing.T) {
	extension := newDependencyDescriptorMarshalFixture(t)

	first, err := extension.Marshal()
	require.NoError(t, err)
	firstCopy := append([]byte(nil), first...)
	checkDependencyDescriptorMarshal(t, first, extension.Structure, extension.Descriptor.FrameNumber)

	extension.Descriptor.FrameNumber++
	second, err := extension.Marshal()
	require.NoError(t, err)
	checkDependencyDescriptorMarshal(t, second, extension.Structure, extension.Descriptor.FrameNumber)

	require.Equal(t, firstCopy, first, "a later marshal modified a previously returned buffer")
	require.NotEqual(t, first, second, "different frame numbers produced identical descriptors")
}

func BenchmarkDependencyDescriptorMarshal(b *testing.B) {
	extension := newDependencyDescriptorMarshalFixture(b)
	frameNumber := extension.Descriptor.FrameNumber

	b.ReportAllocs()
	var buf []byte
	for b.Loop() {
		extension.Descriptor.FrameNumber = frameNumber
		frameNumber++

		var err error
		buf, err = extension.Marshal()
		require.NoError(b, err)
	}

	checkDependencyDescriptorMarshal(b, buf, extension.Structure, frameNumber-1)
}

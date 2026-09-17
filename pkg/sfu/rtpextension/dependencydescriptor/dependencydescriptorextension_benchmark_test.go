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

	"github.com/livekit/livekit-server/pkg/sfu/utils"
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

func TestDependencyDescriptorMarshalTo(t *testing.T) {
	extension := newDependencyDescriptorMarshalFixture(t)

	// every template of the fixture structure, i. e. the per-packet descriptors
	// the forwarding path marshals, has to match Marshal byte for byte and fit
	// the inline size the forwarding path reserves
	for i, template := range extension.Structure.Templates {
		extension.Descriptor.FrameDependencies = template
		extension.Descriptor.FrameNumber = uint16(i)

		want, err := extension.Marshal()
		require.NoError(t, err)
		require.LessOrEqual(t, len(want), MaxInlineExtensionSize)

		var scratch [MaxInlineExtensionSize]byte
		n, err := extension.MarshalTo(scratch[:])
		require.NoError(t, err)
		require.Equal(t, want, scratch[:n])
		checkDependencyDescriptorMarshal(t, scratch[:n], extension.Structure, uint16(i))
	}
}

func TestDependencyDescriptorMarshalToBufferTooSmall(t *testing.T) {
	extension := newDependencyDescriptorMarshalFixture(t)

	want, err := extension.Marshal()
	require.NoError(t, err)

	_, err = extension.MarshalTo(make([]byte, len(want)-1))
	require.ErrorIs(t, err, ErrBufferTooSmall)

	n, err := extension.MarshalTo(make([]byte, len(want)))
	require.NoError(t, err)
	require.Equal(t, len(want), n)

	// a descriptor carrying the full dependency structure does not fit inline
	extension.Descriptor.AttachedStructure = extension.Structure
	_, err = extension.MarshalTo(make([]byte, MaxInlineExtensionSize))
	require.ErrorIs(t, err, ErrBufferTooSmall)

	keyFrame, err := extension.Marshal()
	require.NoError(t, err)
	require.Greater(t, len(keyFrame), MaxInlineExtensionSize)
}

// TestDependencyDescriptorMarshalToLargerDescriptors covers the 8 to 16 byte
// band, where the active decode targets bitmask and custom frame dependencies
// land and where the sequencer's own inline copy already spills.
func TestDependencyDescriptorMarshalToLargerDescriptors(t *testing.T) {
	extension := newDependencyDescriptorMarshalFixture(t)
	bitmask := uint32(0x1ff)

	// the widest per-packet descriptor of the fixture, with the bitmask attached
	widest := 0
	for _, template := range extension.Structure.Templates {
		extension.Descriptor.FrameDependencies = template
		extension.Descriptor.ActiveDecodeTargetsBitmask = &bitmask

		want, err := extension.Marshal()
		require.NoError(t, err)
		widest = max(widest, len(want))

		var scratch [MaxInlineExtensionSize]byte
		n, err := extension.MarshalTo(scratch[:])
		require.NoError(t, err)
		require.Equal(t, want, scratch[:n])
	}
	require.GreaterOrEqual(t, widest, 8)

	// custom frame diffs and chain diffs, i. e. a frame that matches no template
	custom := extension.Structure.Templates[0].Clone()
	custom.FrameDiffs = []int{1, 300, 4000}
	for i := range custom.ChainDiffs {
		custom.ChainDiffs[i] = 200 + i
	}
	extension.Descriptor.FrameDependencies = custom
	extension.Descriptor.ActiveDecodeTargetsBitmask = nil

	want, err := extension.Marshal()
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(want), 8)
	require.LessOrEqual(t, len(want), MaxInlineExtensionSize)

	var scratch [MaxInlineExtensionSize]byte
	n, err := extension.MarshalTo(scratch[:])
	require.NoError(t, err)
	require.Equal(t, want, scratch[:n])
	checkDependencyDescriptorMarshal(t, scratch[:n], extension.Structure, extension.Descriptor.FrameNumber)
}

func TestDependencyDescriptorMarshalAllocs(t *testing.T) {
	if utils.RaceEnabled {
		// the race detector perturbs allocation counts, see #4874
		t.Skip("allocation count is not meaningful under the race detector")
	}

	extension := newDependencyDescriptorMarshalFixture(t)

	var (
		scratch [MaxInlineExtensionSize]byte
		n       int
		err     error
	)
	allocs := testing.AllocsPerRun(100, func() {
		n, err = extension.MarshalTo(scratch[:])
	})
	require.NoError(t, err)
	require.NotZero(t, n)
	require.Zero(t, allocs, "a per-packet descriptor has to marshal without allocating")

	// a key frame descriptor carries the full dependency structure, does not fit
	// the inline scratch and spills to exactly one allocation, the Marshal slice
	extension.Descriptor.AttachedStructure = extension.Structure

	var buf []byte
	allocs = testing.AllocsPerRun(100, func() {
		if n, err = extension.MarshalTo(scratch[:]); err != nil {
			buf, err = extension.Marshal()
		}
	})
	require.NoError(t, err)
	require.Greater(t, len(buf), MaxInlineExtensionSize)
	require.Equal(t, float64(1), allocs, "the spill path has to allocate only the Marshal slice")
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

func BenchmarkDependencyDescriptorMarshalTo(b *testing.B) {
	extension := newDependencyDescriptorMarshalFixture(b)
	frameNumber := extension.Descriptor.FrameNumber

	b.ReportAllocs()
	var (
		scratch [MaxInlineExtensionSize]byte
		n       int
	)
	for b.Loop() {
		extension.Descriptor.FrameNumber = frameNumber
		frameNumber++

		var err error
		n, err = extension.MarshalTo(scratch[:])
		require.NoError(b, err)
	}

	checkDependencyDescriptorMarshal(b, scratch[:n], extension.Structure, frameNumber-1)
}

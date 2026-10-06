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

package dependencydescriptor

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDependencyDescriptorUnmarshal(t *testing.T) {

	// hex bytes from traffic capture
	hexes := []string{
		"c1017280081485214eafffaaaa863cf0430c10c302afc0aaa0063c00430010c002a000a80006000040001d954926e082b04a0941b820ac1282503157f974000ca864330e222222eca8655304224230eca877530077004200ef008601df010d",
		"86017340fc",
		"46017340fc",
		"c3017540fc",
		"88017640fc",
		"48017640fc",
		"c2017840fc",
		//
		"c1017280081485214eafffaaaa863cf0430c10c302afc0aaa0063c00430010c002a000a80006000040001d954926e082b04a0941b820ac1282503157f974000ca864330e222222eca8655304224230eca877530077004200ef008601df010d",
		"860173",
		"460173",
		"8b0174",
		"0b0174",
		"0b0174",
		"c30175",
	}

	var structure *FrameDependencyStructure

	for _, h := range hexes {
		buf, err := hex.DecodeString(h)
		if err != nil {
			t.Fatal(err)
		}

		var ddVal DependencyDescriptor
		var d = DependencyDescriptorExtension{
			Structure:  structure,
			Descriptor: &ddVal,
		}
		if _, err := d.Unmarshal(buf); err != nil {
			t.Fatal(err)
		}
		if ddVal.AttachedStructure != nil {
			structure = ddVal.AttachedStructure
		}

		t.Log(ddVal.String())
	}
}

// Unmarshal into a descriptor whose FrameDependencies and Resolution are set writes into that
// storage, gives the same result as a fresh descriptor, and leaves the structure's templates intact
func TestDependencyDescriptorUnmarshalIntoStorage(t *testing.T) {
	hexes := []string{
		"c1017280081485214eafffaaaa863cf0430c10c302afc0aaa0063c00430010c002a000a80006000040001d954926e082b04a0941b820ac1282503157f974000ca864330e222222eca8655304224230eca877530077004200ef008601df010d",
		"86017340fc",
		"46017340fc",
		"c3017540fc",
		"88017640fc",
		"48017640fc",
		"c2017840fc",
	}

	var structure *FrameDependencyStructure
	var fd FrameDependencyTemplate
	var res RenderResolution
	for _, h := range hexes {
		buf, err := hex.DecodeString(h)
		require.NoError(t, err)

		var fresh DependencyDescriptor
		_, err = (&DependencyDescriptorExtension{Structure: structure, Descriptor: &fresh}).Unmarshal(buf)
		require.NoError(t, err)

		reused := DependencyDescriptor{FrameDependencies: &fd, Resolution: &res}
		_, err = (&DependencyDescriptorExtension{Structure: structure, Descriptor: &reused}).Unmarshal(buf)
		require.NoError(t, err)

		require.Same(t, &fd, reused.FrameDependencies)
		require.Equal(t, fresh.FrameDependencies, reused.FrameDependencies)
		require.Equal(t, fresh.Resolution, reused.Resolution)
		require.Equal(t, fresh.String(), reused.String())

		if fresh.AttachedStructure != nil {
			structure = fresh.AttachedStructure
		}
	}

	// the templates are copied out, not shared
	for _, template := range structure.Templates {
		require.NotSame(t, template, &fd)
	}
}

// a caller may hand Unmarshal a descriptor that still points at one of the structure's templates,
// custom fields of the packet must not change that template
func TestDependencyDescriptorUnmarshalDoesNotWriteIntoTemplate(t *testing.T) {
	fixture := newDependencyDescriptorMarshalFixture(t)
	structure := fixture.Structure

	// a per-packet descriptor using a template with frame diffs, with one custom diff
	var template *FrameDependencyTemplate
	for _, tmpl := range structure.Templates {
		if len(tmpl.FrameDiffs) > 0 {
			template = tmpl
			break
		}
	}
	require.NotNil(t, template)
	want := append([]int(nil), template.FrameDiffs...)

	custom := template.Clone()
	custom.FrameDiffs[0] += 2
	buf, err := (&DependencyDescriptorExtension{
		Descriptor: &DependencyDescriptor{FrameNumber: 7, FrameDependencies: custom},
		Structure:  structure,
	}).Marshal()
	require.NoError(t, err)

	parsed := DependencyDescriptor{FrameDependencies: template}
	_, err = (&DependencyDescriptorExtension{Structure: structure, Descriptor: &parsed}).Unmarshal(buf)
	require.NoError(t, err)
	require.Equal(t, custom.FrameDiffs, parsed.FrameDependencies.FrameDiffs)
	require.NotSame(t, template, parsed.FrameDependencies)
	require.Equal(t, want, template.FrameDiffs)
}

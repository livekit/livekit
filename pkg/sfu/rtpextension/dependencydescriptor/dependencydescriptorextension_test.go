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

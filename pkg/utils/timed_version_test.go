/*
 * Copyright 2022 LiveKit, Inc
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package utils

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTimedVersion(t *testing.T) {
	gen := NewDefaultTimedVersionGenerator()
	tv1 := gen.New()
	tv2 := gen.New()
	tv3 := gen.New()

	assert.True(t, tv3.After(tv1))
	assert.True(t, tv3.After(tv2))
	assert.True(t, tv2.After(tv1))

	tv2.Update(tv3)
	assert.True(t, tv2.After(tv1))
	// tv3 and tv2 are equivalent after update
	assert.False(t, tv3.After(tv2))
}

// Copyright 2024 LiveKit, Inc.
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

package videolayerselector

import (
	"testing"

	"math/rand"

	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/sfu/utils"
	"github.com/livekit/protocol/logger"
)

func TestFrameNumberWrapper(t *testing.T) {

	logger.InitFromConfig(&logger.Config{Level: "debug"}, t.Name())

	fnWrap := &FrameNumberWrapper{logger: logger.GetLogger()}

	fnWrapAround := utils.NewWrapAround[uint16, uint64](utils.WrapAroundParams{IsRestartAllowed: false})

	firstF := uint16(1000)

	testFrameOrder := func(frame uint16, isKeyFrame bool, frame2 uint16, isKeyFrame2, expectInorder bool) {
		frameUnwrap := fnWrapAround.Update(frame).ExtendedVal
		wrappedFrame := uint16(fnWrap.UpdateAndGet(frameUnwrap, isKeyFrame))

		// make sure wrap around always get in order frame number
		fnWrapAround.Update(frame + (frame2-frame)/2)

		frame2Unwrap := fnWrapAround.Update(frame2).ExtendedVal
		wrappedFrame2 := uint16(fnWrap.UpdateAndGet(frame2Unwrap, isKeyFrame2))
		// keeps order
		require.Equal(t, expectInorder, inOrder(wrappedFrame2, wrappedFrame), "frame %d, frame2 %d, wrappedFrame %d, wrapped Frame2 %d, frameUnwrap %d, frame2Unwrap %d", frame, frame2, wrappedFrame, wrappedFrame2, frameUnwrap, frame2Unwrap)
		// frame number diff should be the same if frame2 is not a key frame
		if !isKeyFrame2 {
			require.Equal(t, frame2-frame, wrappedFrame2-wrappedFrame)
		}
	}

	secondF := getFrame(firstF, true)
	testFrameOrder(firstF, true, secondF, false, true)

	// non key frame keeps diff and order
	for i := 0; i < 100; i++ {
		// frame in order
		firstF = secondF
		secondF = getFrame(firstF, true)
		testFrameOrder(firstF, false, secondF, false, true)

		// frame out of order
		firstF = secondF
		secondF = getFrame(firstF, false)
		// it is possile that an out of order non-keyframe has been converted to in order frame number if the diff is 32768
		// that is ok because the client can't decode in such case and always need to wait for the key frame.
		// so it is just a failure of test case and increase the frame number here.
		if secondF-firstF == 0x8000 {
			secondF++
		}
		testFrameOrder(firstF, false, secondF, false, false)

		// key frame in order
		firstF = secondF
		secondF = getFrame(firstF, true)
		testFrameOrder(firstF, false, secondF, true, true)

		// frame in order
		firstF = secondF
		secondF = getFrame(firstF, true)
		testFrameOrder(firstF, false, secondF, false, true)

		// key frame out of order but should be in order after wrap around
		firstF = secondF
		secondF = getFrame(firstF, false)
		testFrameOrder(firstF, false, secondF, true, true)
	}
}

func inOrder(a, b uint16) bool {
	return a-b < 0x8000 || (a-b == 0x8000 && a > b)
}

func getFrame(base uint16, inorder bool) uint16 {
	if inorder {
		return base + uint16(rand.Intn(0x8000))
	}

	for {
		ret := base + uint16(rand.Intn(0x8000)) + 0x8000
		if !inOrder(ret, base) {
			return ret
		}
	}
}

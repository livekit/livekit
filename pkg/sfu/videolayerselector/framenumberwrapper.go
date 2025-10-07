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

import "github.com/livekit/protocol/logger"

type FrameNumberWrapper struct {
	offset uint64
	last   uint64
	inited bool
	logger logger.Logger
}

// UpdateAndGet returns the wrapped frame number from the given frame number, and updates the offset to
// make sure the returned frame number is always inorder. Should only updateOffset if the new frame is a keyframe
// because frame dependencies uses on the frame number diff so frames inside a GOP should have the same offset.
func (f *FrameNumberWrapper) UpdateAndGet(new uint64, updateOffset bool) uint64 {
	if !f.inited {
		f.last = new
		f.inited = true
		return new
	}

	if new <= f.last {
		return new + f.offset
	}

	if updateOffset {
		new16 := uint16(new + f.offset)
		last16 := uint16(f.last + f.offset)
		// if new frame number wraps around and is considered as earlier by client, increase offset to make it later
		if diff := new16 - last16; diff > 0x8000 || (diff == 0x8000 && new16 < last16) {
			// increase offset by 6000, nearly 10 seconds for 30fps video with 3 spatial layers
			prevOffset := f.offset
			f.offset += uint64(65535 - diff + 6000)

			f.logger.Debugw("wrap around frame number seen, update offset", "new", new, "last", f.last, "offset", f.offset, "prevOffset", prevOffset, "lastWrapFn", last16, "newWrapFn", new16)
		}
	}
	f.last = new
	return new + f.offset
}

func (f *FrameNumberWrapper) LastOrigin() uint64 {
	return f.last
}

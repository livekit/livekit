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

package buffer

import (
	"testing"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/logger"

	dd "github.com/livekit/livekit-server/pkg/sfu/rtpextension/dependencydescriptor"
)

const ddTestExtID = uint8(1)

// single spatial layer, single temporal layer, one decode target, one chain
func newL1T1Structure(structureID int) *dd.FrameDependencyStructure {
	return &dd.FrameDependencyStructure{
		StructureId:                  structureID,
		NumDecodeTargets:             1,
		NumChains:                    1,
		DecodeTargetProtectedByChain: []int{0},
		Templates: []*dd.FrameDependencyTemplate{
			{ // key frame
				DecodeTargetIndications: []dd.DecodeTargetIndication{dd.DecodeTargetSwitch},
				ChainDiffs:              []int{0},
			},
			{ // delta frame
				DecodeTargetIndications: []dd.DecodeTargetIndication{dd.DecodeTargetRequired},
				FrameDiffs:              []int{1},
				ChainDiffs:              []int{1},
			},
		},
	}
}

type ddTestFeeder struct {
	t      *testing.T
	parser *DependencyDescriptorParser
	// structure the writer marshals against, i. e. what the publisher last sent
	structure *dd.FrameDependencyStructure
	seq       uint16
}

func newDDTestFeeder(t *testing.T) *ddTestFeeder {
	return &ddTestFeeder{
		t:      t,
		parser: NewDependencyDescriptorParser(ddTestExtID, logger.GetLogger(), func(int32, int32) {}, false),
	}
}

// keyFrame sends a key frame carrying `structure`. Passing the same structure id as
// the previous key frame models a publisher that repeats an unchanged structure,
// which is what screen content with frequent key frames does.
func (f *ddTestFeeder) keyFrame(frameNumber uint16, structure *dd.FrameDependencyStructure) (*ExtDependencyDescriptor, error) {
	f.structure = structure
	return f.feed(frameNumber, structure.Templates[0], structure)
}

func (f *ddTestFeeder) deltaFrame(frameNumber uint16) (*ExtDependencyDescriptor, error) {
	return f.feed(frameNumber, f.structure.Templates[1], nil)
}

func (f *ddTestFeeder) feed(
	frameNumber uint16,
	template *dd.FrameDependencyTemplate,
	attachedStructure *dd.FrameDependencyStructure,
) (*ExtDependencyDescriptor, error) {
	f.t.Helper()

	ddVal := &dd.DependencyDescriptor{
		FirstPacketInFrame: true,
		LastPacketInFrame:  true,
		FrameNumber:        frameNumber,
		FrameDependencies:  template,
		AttachedStructure:  attachedStructure,
	}
	buf, err := (&dd.DependencyDescriptorExtension{Descriptor: ddVal, Structure: f.structure}).Marshal()
	require.NoError(f.t, err)

	f.seq++
	pkt := &rtp.Packet{Header: rtp.Header{SequenceNumber: f.seq}}
	require.NoError(f.t, pkt.SetExtension(ddTestExtID, buf))

	extDD, _, err := f.parser.Parse(pkt)
	return extDD, err
}

// A key frame that repeats the current structure must not start dropping frames that
// precede it. Upstream advanced the drop threshold on every structure bearing key
// frame, so with frequent key frames (screen content) a late or retransmitted frame
// arriving after one was discarded as "earlier than current structure".
func TestDependencyDescriptorParserLateFrameAfterRepeatedStructure(t *testing.T) {
	f := newDDTestFeeder(t)
	structure := newL1T1Structure(0)

	_, err := f.keyFrame(0, structure)
	require.NoError(t, err)
	_, err = f.deltaFrame(1)
	require.NoError(t, err)
	_, err = f.deltaFrame(3)
	require.NoError(t, err)

	// key frame repeating the same structure id
	_, err = f.keyFrame(4, structure)
	require.NoError(t, err)

	// frame 2 finally shows up (reordered or retransmitted). The structure has not
	// changed, so it is still decodable and must be forwarded.
	extDD, err := f.deltaFrame(2)
	require.NoError(t, err)
	require.NotNil(t, extDD)
	require.EqualValues(t, 2, extDD.ExtFrameNum)
}

// A key frame carrying a *different* structure does invalidate everything before it:
// earlier frames reference templates that no longer exist.
func TestDependencyDescriptorParserLateFrameAfterStructureChange(t *testing.T) {
	f := newDDTestFeeder(t)

	_, err := f.keyFrame(0, newL1T1Structure(0))
	require.NoError(t, err)
	_, err = f.deltaFrame(1)
	require.NoError(t, err)
	_, err = f.deltaFrame(3)
	require.NoError(t, err)

	_, err = f.keyFrame(4, newL1T1Structure(1))
	require.NoError(t, err)

	_, err = f.deltaFrame(2)
	require.ErrorIs(t, err, ErrFrameEarlierThanKeyFrame)
}

// An out-of-order key frame must still be dropped: accepting it would regress
// structureExtFrameNum (ExtKeyFrameNum) and replay a stale structure update.
func TestDependencyDescriptorParserOutOfOrderKeyFrame(t *testing.T) {
	f := newDDTestFeeder(t)
	structure := newL1T1Structure(0)

	_, err := f.keyFrame(0, structure)
	require.NoError(t, err)
	_, err = f.deltaFrame(1)
	require.NoError(t, err)
	_, err = f.keyFrame(4, structure)
	require.NoError(t, err)

	_, err = f.keyFrame(2, structure)
	require.ErrorIs(t, err, ErrFrameEarlierThanKeyFrame)
}

// ExtKeyFrameNum keeps tracking every structure bearing key frame, unchanged by the
// split of the drop threshold into its own field.
func TestDependencyDescriptorParserExtKeyFrameNum(t *testing.T) {
	f := newDDTestFeeder(t)
	structure := newL1T1Structure(0)

	extDD, err := f.keyFrame(0, structure)
	require.NoError(t, err)
	require.EqualValues(t, 0, extDD.ExtKeyFrameNum)

	extDD, err = f.deltaFrame(1)
	require.NoError(t, err)
	require.EqualValues(t, 0, extDD.ExtKeyFrameNum)

	// repeated structure still advances ExtKeyFrameNum
	extDD, err = f.keyFrame(4, structure)
	require.NoError(t, err)
	require.EqualValues(t, 4, extDD.ExtKeyFrameNum)

	extDD, err = f.deltaFrame(5)
	require.NoError(t, err)
	require.EqualValues(t, 4, extDD.ExtKeyFrameNum)
}

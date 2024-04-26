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

package videolayerselector

import (
	"sort"
	"testing"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	dd "github.com/livekit/livekit-server/pkg/sfu/rtpextension/dependencydescriptor"
	"github.com/livekit/protocol/logger"
)

func TestDecodeTarget(t *testing.T) {
	target := buffer.DependencyDescriptorDecodeTarget{
		Target: 1,
		Layer:  buffer.VideoLayer{Spatial: 1, Temporal: 2},
	}

	t.Run("No Chain", func(t *testing.T) {
		dt := NewDecodeTarget(target, nil)
		require.True(t, dt.Valid())
		// no indication found
		_, err := dt.OnFrame(1, &dd.FrameDependencyTemplate{
			DecodeTargetIndications: []dd.DecodeTargetIndication{},
		})
		require.Error(t, err)

		ret, err := dt.OnFrame(1, &dd.FrameDependencyTemplate{
			DecodeTargetIndications: []dd.DecodeTargetIndication{dd.DecodeTargetNotPresent, dd.DecodeTargetRequired},
		})
		require.NoError(t, err)
		require.True(t, ret.TargetValid)
		require.Equal(t, dd.DecodeTargetRequired, ret.DTI)
	})

	t.Run("With Chain", func(t *testing.T) {
		decisions := NewSelectorDecisionCache(256, 80)
		chain := NewFrameChain(decisions, 1, logger.GetLogger())
		dt := NewDecodeTarget(target, chain)
		chain.BeginUpdateActive()
		dt.UpdateActive(1 << dt.Target)
		chain.EndUpdateActive()
		require.True(t, dt.Active())
		require.False(t, dt.Valid())

		// chain intact
		frame := &dd.FrameDependencyTemplate{
			DecodeTargetIndications: []dd.DecodeTargetIndication{dd.DecodeTargetNotPresent, dd.DecodeTargetRequired},
			ChainDiffs:              []int{0, 0},
		}
		chain.OnFrame(1, frame)
		require.True(t, dt.Valid())
		ret, err := dt.OnFrame(1, frame)
		require.NoError(t, err)
		require.True(t, ret.TargetValid)
		require.Equal(t, dd.DecodeTargetRequired, ret.DTI)

	})
}

func TestFrameChain(t *testing.T) {
	decisions := NewSelectorDecisionCache(256, 3)
	chain := NewFrameChain(decisions, 0, logger.GetLogger())
	require.True(t, chain.Broken())

	// chain intact
	frameNoDiff := &dd.FrameDependencyTemplate{
		ChainDiffs: []int{0},
	}
	// not active
	require.False(t, chain.OnFrame(1, frameNoDiff))

	chain.BeginUpdateActive()
	chain.UpdateActive(true)
	chain.EndUpdateActive()

	require.True(t, chain.OnFrame(1, frameNoDiff))
	decisions.AddForwarded(1)

	frameDiff1 := &dd.FrameDependencyTemplate{
		ChainDiffs: []int{1},
	}

	require.True(t, chain.OnFrame(2, frameDiff1))
	decisions.AddForwarded(2)

	// frame 5 arrives first , but frame 4 can be recovered by NACK
	require.True(t, chain.OnFrame(5, frameDiff1))
	decisions.AddForwarded(5)

	// frame 4 arrives, chain remains intact
	require.True(t, chain.OnFrame(4, frameDiff1))
	decisions.AddForwarded(4)

	// frame 3 missed by out of nack range, chain broken
	decisions.AddForwarded(7)
	require.True(t, chain.Broken())

	// recovery by non-diff frame
	require.True(t, chain.OnFrame(1000, frameNoDiff))
	require.False(t, chain.Broken())
	decisions.AddForwarded(1000)

	// broken by dropped frame
	require.True(t, chain.OnFrame(1002, frameDiff1))
	decisions.AddDropped(1001)
	require.True(t, chain.Broken())

	// recovery by non-diff frame
	require.True(t, chain.OnFrame(2000, frameNoDiff))
	decisions.AddForwarded(2000)
	decisions.AddDropped(2001)
	require.False(t, chain.OnFrame(2002, frameDiff1))
	require.True(t, chain.Broken())
}

func TestDependencyDescriptor(t *testing.T) {
	ddSelector := NewDependencyDescriptor(logger.GetLogger())
	targetLayer := buffer.VideoLayer{Spatial: 1, Temporal: 2}
	ddSelector.SetTarget(targetLayer)
	ddSelector.SetRequestSpatial(1)

	// no dd ext, dropped
	ret := ddSelector.Select(&buffer.ExtPacket{Packet: &rtp.Packet{}}, 0)
	require.False(t, ret.IsSelected)
	require.False(t, ret.IsRelevant)

	// non key frame, dropped
	ret = ddSelector.Select(&buffer.ExtPacket{
		KeyFrame: false,
		DependencyDescriptor: &buffer.ExtDependencyDescriptor{
			Descriptor: &dd.DependencyDescriptor{
				FrameNumber: 1,
				FrameDependencies: &dd.FrameDependencyTemplate{
					SpatialId:  int(targetLayer.Spatial),
					TemporalId: int(targetLayer.Temporal),
				},
			},
		},
		Packet: &rtp.Packet{},
	}, 0)
	require.False(t, ret.IsSelected)
	require.False(t, ret.IsRelevant)

	frames := createDDFrames(buffer.VideoLayer{Spatial: 2, Temporal: 2}, 3)
	// key frame, update structure and decode targets
	ret = ddSelector.Select(frames[0], 0)
	require.True(t, ret.IsSelected)
	require.Equal(t, ddSelector.GetCurrent(), ddSelector.GetTarget())
	sync, _ := ddSelector.CheckSync()
	require.True(t, sync)

	// forward frame belongs to target layer
	// drop frame exceeds target layer (not present in target layer or lower layer)
	// forward frame not present in target layer but present in lower layer
	var (
		belongTargetCase bool
		exceedTargetCase bool
		lowerTargetCase  bool
	)
	idx := 1
	var frameForwarded, frameDropped []*buffer.ExtPacket
	for ; idx < len(frames); idx++ {
		fd := frames[idx].DependencyDescriptor.Descriptor.FrameDependencies
		ret = ddSelector.Select(frames[idx], 0)
		switch {
		case fd.SpatialId == int(targetLayer.Spatial) && fd.TemporalId == int(targetLayer.Temporal):
			require.True(t, ret.IsSelected)
			belongTargetCase = true
			frameForwarded = append(frameForwarded, frames[idx])
		case fd.SpatialId < int(targetLayer.Spatial) && fd.TemporalId == 0:
			require.True(t, ret.IsSelected)
			lowerTargetCase = true
			frameForwarded = append(frameForwarded, frames[idx])
		case fd.SpatialId > int(targetLayer.Spatial) || fd.TemporalId > int(targetLayer.Temporal):
			require.False(t, ret.IsSelected)
			exceedTargetCase = true
			frameDropped = append(frameDropped, frames[idx])
		}

		if belongTargetCase && exceedTargetCase && lowerTargetCase {
			break
		}
	}

	require.True(t, belongTargetCase && exceedTargetCase && lowerTargetCase)

	// select frame already forwarded
	ret = ddSelector.Select(frameForwarded[0], 0)
	require.True(t, ret.IsSelected)

	// drop frame already dropped
	ret = ddSelector.Select(frameDropped[0], 0)
	require.False(t, ret.IsSelected)

	// drop frame present but not decodable (dependency frame missed)
	idx++
	for ; idx < len(frames); idx++ {
		fd := frames[idx].DependencyDescriptor.Descriptor.FrameDependencies
		ret = ddSelector.Select(frames[idx], 0)
		if fd.SpatialId == int(targetLayer.Spatial) && fd.TemporalId == int(targetLayer.Temporal) {
			break
		}
	}
	notDecodableFrame := frames[idx]
	notDecodableFrame.DependencyDescriptor.Descriptor.FrameDependencies.FrameDiffs = []int{
		int(notDecodableFrame.DependencyDescriptor.Descriptor.FrameNumber - frameDropped[0].DependencyDescriptor.Descriptor.FrameNumber),
	}
	ret = ddSelector.Select(notDecodableFrame, 0)
	require.False(t, ret.IsSelected)

	// target layer broken
	idx++
	for ; idx < len(frames); idx++ {
		fd := frames[idx].DependencyDescriptor.Descriptor.FrameDependencies
		ret = ddSelector.Select(frames[idx], 0)
		if fd.SpatialId == int(targetLayer.Spatial) && fd.TemporalId == int(targetLayer.Temporal) {
			break
		}
	}
	brokenFrame := frames[idx]
	brokenFrame.DependencyDescriptor.Descriptor.FrameDependencies.ChainDiffs[targetLayer.Spatial] =
		int(notDecodableFrame.DependencyDescriptor.Descriptor.FrameNumber - frameDropped[0].DependencyDescriptor.Descriptor.FrameNumber)
	ret = ddSelector.Select(brokenFrame, 0)
	require.False(t, ret.IsSelected)

	// switch to lower layer, forward frame
	idx++
	var switchToLower bool
	for ; idx < len(frames); idx++ {
		ret = ddSelector.Select(frames[idx], 0)
		if ret.IsSelected {
			require.True(t, targetLayer.GreaterThan(ddSelector.GetCurrent()))
			switchToLower = true
			break
		}
	}
	require.True(t, switchToLower)

	// not sync with requested layer
	ddSelector.SetRequestSpatial(targetLayer.Spatial)
	locked, layer := ddSelector.CheckSync()
	require.False(t, locked)
	require.Equal(t, targetLayer.Spatial, layer)
	// request to current layer, sync
	ddSelector.SetRequestSpatial(ddSelector.GetCurrent().Spatial)
	locked, _ = ddSelector.CheckSync()
	require.True(t, locked)

	// should drop frame that relies on a keyframe is not present in current selection
	framesPrevious := createDDFrames(buffer.VideoLayer{Spatial: 2, Temporal: 2}, 1000)
	ret = ddSelector.Select(framesPrevious[1], 0)
	require.False(t, ret.IsSelected)
	// keyframe lost, out of sync
	locked, _ = ddSelector.CheckSync()
	require.False(t, locked)
}

func createDDFrames(maxLayer buffer.VideoLayer, startFrameNumber uint16) []*buffer.ExtPacket {
	var frames []*buffer.ExtPacket
	var activeBitMask uint32
	var decodeTargets []buffer.DependencyDescriptorDecodeTarget
	var decodeTargetsProtectByChain []int
	for i := 0; i <= int(maxLayer.Spatial); i++ {
		for j := 0; j <= int(maxLayer.Temporal); j++ {
			decodeTargets = append(decodeTargets, buffer.DependencyDescriptorDecodeTarget{
				Target: i*int(maxLayer.Temporal+1) + j,
				Layer:  buffer.VideoLayer{Spatial: int32(i), Temporal: int32(j)},
			})
			decodeTargetsProtectByChain = append(decodeTargetsProtectByChain, i)
			activeBitMask |= 1 << uint(i*int(maxLayer.Temporal+1)+j)
		}
	}
	sort.Slice(decodeTargets, func(i, j int) bool {
		return decodeTargets[i].Layer.GreaterThan(decodeTargets[j].Layer)
	})

	chainDiffs := make([]int, int(maxLayer.Spatial)+1)
	dtis := make([]dd.DecodeTargetIndication, len(decodeTargets))
	for _, dt := range decodeTargets {
		dtis[dt.Target] = dd.DecodeTargetSwitch
	}

	templates := make([]*dd.FrameDependencyTemplate, len(decodeTargets))

	for _, dt := range decodeTargets {
		templates[dt.Target] = &dd.FrameDependencyTemplate{
			SpatialId:               int(dt.Layer.Spatial),
			TemporalId:              int(dt.Layer.Temporal),
			ChainDiffs:              chainDiffs,
			DecodeTargetIndications: dtis,
		}
	}
	keyFrame := &buffer.ExtPacket{
		KeyFrame: true,
		DependencyDescriptor: &buffer.ExtDependencyDescriptor{
			Descriptor: &dd.DependencyDescriptor{
				FrameNumber: startFrameNumber,
				FrameDependencies: &dd.FrameDependencyTemplate{
					SpatialId:               0,
					TemporalId:              0,
					ChainDiffs:              chainDiffs,
					DecodeTargetIndications: dtis,
				},
				AttachedStructure: &dd.FrameDependencyStructure{
					NumDecodeTargets:             int((maxLayer.Spatial + 1) * (maxLayer.Temporal + 1)),
					NumChains:                    int(maxLayer.Spatial) + 1,
					DecodeTargetProtectedByChain: decodeTargetsProtectByChain,
					Templates:                    templates,
				},
				ActiveDecodeTargetsBitmask: &activeBitMask,
			},
			DecodeTargets:              decodeTargets,
			StructureUpdated:           true,
			ActiveDecodeTargetsUpdated: true,
			Integrity:                  true,
			ExtFrameNum:                uint64(startFrameNumber),
			ExtKeyFrameNum:             uint64(startFrameNumber),
		},
		Packet: &rtp.Packet{
			Header: rtp.Header{
				SSRC: 1234,
			},
		},
	}

	frames = append(frames, keyFrame)

	chainPrevFrame := make(map[int]int)
	for i := 0; i <= int(maxLayer.Spatial); i++ {
		chainPrevFrame[i] = int(startFrameNumber)
	}
	startFrameNumber++
	for i := 0; i < 10; i++ {
		for j := len(decodeTargets) - 1; j >= 0; j-- {
			dt := decodeTargets[j]
			frameChainDiffs := make([]int, len(chainDiffs))
			for i := range frameChainDiffs {
				frameChainDiffs[i] = int(startFrameNumber) - chainPrevFrame[i]
			}

			frameDtis := make([]dd.DecodeTargetIndication, len(dtis))
			for k := range frameDtis {
				if k >= dt.Target {
					if dt.Layer.Temporal == 0 {
						frameDtis[k] = dd.DecodeTargetRequired
					} else {
						frameDtis[k] = dd.DecodeTargetDiscardable
					}
				} else {
					frameDtis[k] = dd.DecodeTargetNotPresent
				}
			}

			frame := &buffer.ExtPacket{
				DependencyDescriptor: &buffer.ExtDependencyDescriptor{
					Descriptor: &dd.DependencyDescriptor{
						FrameNumber: startFrameNumber,
						FrameDependencies: &dd.FrameDependencyTemplate{
							SpatialId:               int(dt.Layer.Spatial),
							TemporalId:              int(dt.Layer.Temporal),
							ChainDiffs:              frameChainDiffs,
							DecodeTargetIndications: frameDtis,
						},
					},
					DecodeTargets:  decodeTargets,
					Integrity:      true,
					ExtFrameNum:    uint64(startFrameNumber),
					ExtKeyFrameNum: keyFrame.DependencyDescriptor.ExtFrameNum,
				},
				Packet: &rtp.Packet{
					Header: rtp.Header{
						SSRC: 1234,
					},
				},
			}

			startFrameNumber++

			if dt.Layer.Temporal == 0 {
				chainPrevFrame[int(dt.Layer.Spatial)] = int(startFrameNumber)
			}

			frames = append(frames, frame)
		}
	}

	return frames
}

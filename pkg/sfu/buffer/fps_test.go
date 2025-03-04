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

package buffer

import (
	"testing"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"

	dd "github.com/livekit/livekit-server/pkg/sfu/rtpextension/dependencydescriptor"
	"github.com/livekit/protocol/logger"
)

type testFrameInfo struct {
	header      rtp.Header
	framenumber uint16
	spatial     int
	temporal    int
	frameDiff   []int
}

func (f *testFrameInfo) toVP8() *ExtPacket {
	return &ExtPacket{
		Packet: &rtp.Packet{Header: f.header},
		Payload: VP8{
			PictureID: f.framenumber,
		},
		VideoLayer: VideoLayer{Spatial: InvalidLayerSpatial, Temporal: int32(f.temporal)},
	}
}

func (f *testFrameInfo) toDD() *ExtPacket {
	return &ExtPacket{
		Packet: &rtp.Packet{Header: f.header},
		DependencyDescriptor: &ExtDependencyDescriptor{
			Descriptor: &dd.DependencyDescriptor{
				FrameNumber: f.framenumber,
				FrameDependencies: &dd.FrameDependencyTemplate{
					FrameDiffs: f.frameDiff,
				},
			},
		},
		VideoLayer: VideoLayer{Spatial: int32(f.spatial), Temporal: int32(f.temporal)},
	}
}

func (f *testFrameInfo) toH26x() *ExtPacket {
	return &ExtPacket{
		Packet:     &rtp.Packet{Header: f.header},
		VideoLayer: VideoLayer{Spatial: InvalidLayerSpatial, Temporal: int32(f.temporal)},
	}
}

func createFrames(startFrameNumber uint16, startTs uint32, startSeq uint16, totalFramesPerSpatial int, fps [][]float32, spatialDependency bool) [][]*testFrameInfo {
	spatials := len(fps)
	temporals := len(fps[0])
	frames := make([][]*testFrameInfo, spatials)
	for s := 0; s < spatials; s++ {
		frames[s] = make([]*testFrameInfo, 0, totalFramesPerSpatial)
	}
	fn := startFrameNumber

	nextTs := make([][]uint32, spatials)
	tsStep := make([][]uint32, spatials)
	for i := 0; i < spatials; i++ {
		nextTs[i] = make([]uint32, temporals)
		tsStep[i] = make([]uint32, temporals)
		for j := 0; j < temporals; j++ {
			nextTs[i][j] = startTs
			tsStep[i][j] = uint32(90000 / fps[i][j])
		}
	}

	currentTs := make([]uint32, spatials)
	for i := 0; i < spatials; i++ {
		currentTs[i] = startTs
	}
	for i := 0; i < totalFramesPerSpatial; i++ {
		for s := 0; s < spatials; s++ {
			frame := &testFrameInfo{
				header:      rtp.Header{Timestamp: currentTs[s], SequenceNumber: startSeq},
				framenumber: fn,
				spatial:     s,
			}
			for t := 0; t < temporals; t++ {
				if currentTs[s] >= nextTs[s][t] {
					frame.temporal = t
					for nt := t; nt < temporals; nt++ {
						nextTs[s][nt] += tsStep[s][nt]
					}
					break
				}
			}
			currentTs[s] += tsStep[s][temporals-1]
			frames[s] = append(frames[s], frame)
			fn++
			startSeq++

			for fidx := len(frames[s]) - 1; fidx >= 0; fidx-- {
				cf := frames[s][fidx]
				if cf.header.Timestamp-frame.header.Timestamp > 0x80000000 {
					frame.frameDiff = append(frame.frameDiff, int(frame.framenumber-cf.framenumber))
					break
				}
			}

			if spatialDependency && frame.spatial > 0 {
				for fidx := len(frames[frame.spatial-1]) - 1; fidx >= 0; fidx-- {
					cf := frames[frame.spatial-1][fidx]
					if cf.header.Timestamp == frame.header.Timestamp {
						frame.frameDiff = append(frame.frameDiff, int(frame.framenumber-cf.framenumber))
						break
					}
				}
			}
		}
	}

	return frames
}

func verifyFps(t *testing.T, expect, got []float32) {
	require.Equal(t, len(expect), len(got))
	for i := 0; i < len(expect); i++ {
		require.GreaterOrEqual(t, got[i], expect[i]*0.9, "expect %v, got %v", expect, got)
		require.LessOrEqual(t, got[i], expect[i]*1.1, "expect %v, got %v", expect, got)
	}
}

type testcase struct {
	startTs           uint32
	startSeq          uint16
	startFrameNumber  uint16
	fps               [][]float32
	spatialDependency bool
}

func TestFpsVP8(t *testing.T) {
	cases := map[string]testcase{
		"normal": {
			startTs:          12345678,
			startFrameNumber: 100,
			fps:              [][]float32{{5, 10, 15}, {5, 10, 15}, {7.5, 15, 30}},
		},
		"frame number and timestamp wrap": {
			startTs:          (uint32(1) << 31) - 10,
			startFrameNumber: (uint16(1) << 15) - 10,
			fps:              [][]float32{{5, 10, 15}, {5, 10, 15}, {7.5, 15, 30}},
		},
		"2 temporal layers": {
			startTs:          12345678,
			startFrameNumber: 100,
			fps:              [][]float32{{7.5, 15}, {7.5, 15}, {15, 30}},
		},
	}

	for name, c := range cases {
		testCase := c
		t.Run(name, func(t *testing.T) {
			fps := testCase.fps
			frames := make([][]*testFrameInfo, 0)
			vp8calcs := make([]*FrameRateCalculatorVP8, len(fps))
			for i := range vp8calcs {
				vp8calcs[i] = NewFrameRateCalculatorVP8(90000, logger.GetLogger())
				frames = append(frames, createFrames(c.startFrameNumber, c.startTs, 10, 200, [][]float32{fps[i]}, false)[0])
			}

			var frameratesGot bool
			for s, fs := range frames {
				for _, f := range fs {
					if vp8calcs[s].RecvPacket(f.toVP8()) {
						frameratesGot = true
						for _, calc := range vp8calcs {
							if !calc.Completed() {
								frameratesGot = false
								break
							}
						}
					}
				}
			}
			require.True(t, frameratesGot)
			for i, calc := range vp8calcs {
				fpsExpected := fps[i]
				fpsGot := calc.GetFrameRate()
				verifyFps(t, fpsExpected, fpsGot[:len(fpsExpected)])
			}
		})
	}
	t.Run("packet lost and duplicate", func(t *testing.T) {
		fps := [][]float32{{7.5, 15}, {7.5, 15}, {15, 30}}
		frames := make([][]*testFrameInfo, 0)
		vp8calcs := make([]*FrameRateCalculatorVP8, len(fps))
		for i := range vp8calcs {
			vp8calcs[i] = NewFrameRateCalculatorVP8(90000, logger.GetLogger())
			frames = append(frames, createFrames(100, 12345678, 10, 300, [][]float32{fps[i]}, false)[0])
			for j := 5; j < 130; j++ {
				if j%2 == 0 {
					frames[i][j] = frames[i][j-1]
				}
			}
		}

		var frameratesGot bool
		for s, fs := range frames {
			for _, f := range fs {
				if vp8calcs[s].RecvPacket(f.toVP8()) {
					frameratesGot = true
					for _, calc := range vp8calcs {
						if !calc.Completed() {
							frameratesGot = false
							break
						}
					}
				}
			}
		}
		require.True(t, frameratesGot)
		for i, calc := range vp8calcs {
			fpsExpected := fps[i]
			fpsGot := calc.GetFrameRate()
			verifyFps(t, fpsExpected, fpsGot[:len(fpsExpected)])
		}
	})
}

func TestFpsDD(t *testing.T) {
	cases := map[string]testcase{
		"normal": {
			startTs:           12345678,
			startFrameNumber:  100,
			fps:               [][]float32{{5.1, 10.1, 16}, {5.1, 10.1, 16}, {8, 15, 30.1}},
			spatialDependency: true,
		},
		"frame number and timestamp wrap": {
			startTs:           (uint32(1) << 31) - 10,
			startFrameNumber:  (uint16(1) << 15) - 10,
			fps:               [][]float32{{7.5, 15, 30}, {7.5, 15, 30}, {7.5, 15, 30}},
			spatialDependency: true,
		},
		"vp8": {
			startTs:           12345678,
			startFrameNumber:  100,
			fps:               [][]float32{{7.5, 15}, {7.5, 15}, {15, 30}},
			spatialDependency: false,
		},
	}

	for name, c := range cases {
		testCase := c
		t.Run(name, func(t *testing.T) {
			fps := testCase.fps
			frames := createFrames(c.startFrameNumber, c.startTs, 10, 500, fps, testCase.spatialDependency)
			ddcalc := NewFrameRateCalculatorDD(90000, logger.GetLogger())
			ddcalc.SetMaxLayer(int32(len(fps)-1), int32(len(fps[0])-1))
			ddcalcs := make([]FrameRateCalculator, len(fps))
			for i := range fps {
				ddcalcs[i] = ddcalc.GetFrameRateCalculatorForSpatial(int32(i))
			}

			var frameratesGot bool
			for s, fs := range frames {
				for _, f := range fs {
					if ddcalcs[s].RecvPacket(f.toDD()) {
						frameratesGot = true
						for _, calc := range ddcalcs {
							if !calc.Completed() {
								frameratesGot = false
								break
							}
						}
					}
				}
			}
			require.True(t, frameratesGot)
			for i, calc := range ddcalcs {
				fpsExpected := fps[i]
				fpsGot := calc.GetFrameRate()
				verifyFps(t, fpsExpected, fpsGot[:len(fpsExpected)])
			}
		})
	}

	t.Run("packet lost and duplicate", func(t *testing.T) {
		fps := [][]float32{{7.5, 15, 30}, {7.5, 15, 30}, {7.5, 15, 30}}
		frames := createFrames(100, 12345678, 10, 500, fps, true)
		ddcalc := NewFrameRateCalculatorDD(90000, logger.GetLogger())
		ddcalc.SetMaxLayer(int32(len(fps)-1), int32(len(fps[0])-1))
		ddcalcs := make([]FrameRateCalculator, len(fps))
		for i := range fps {
			ddcalcs[i] = ddcalc.GetFrameRateCalculatorForSpatial(int32(i))
			for j := 5; j < 130; j++ {
				if j%2 == 0 {
					frames[i][j] = frames[i][j-1]
				}
			}
		}

		var frameratesGot bool
		for s, fs := range frames {
			for _, f := range fs {
				if ddcalcs[s].RecvPacket(f.toDD()) {
					frameratesGot = true
					for _, calc := range ddcalcs {
						if !calc.Completed() {
							frameratesGot = false
							break
						}
					}
				}
			}
		}
		require.True(t, frameratesGot)
		for i, calc := range ddcalcs {
			fpsExpected := fps[i]
			fpsGot := calc.GetFrameRate()
			verifyFps(t, fpsExpected, fpsGot[:len(fpsExpected)])
		}
	})
}

func TestFpsH26x(t *testing.T) {
	cases := map[string]testcase{
		"normal": {
			startTs:          12345678,
			startSeq:         100,
			startFrameNumber: 100,
			fps:              [][]float32{{5, 10, 15}, {5, 10, 15}, {7.5, 15, 30}},
		},
		"frame number and timestamp wrap": {
			startTs:          (uint32(1) << 31) - 10,
			startSeq:         (uint16(1) << 15) - 10,
			startFrameNumber: (uint16(1) << 15) - 10,
			fps:              [][]float32{{5, 10, 15}, {5, 10, 15}, {7.5, 15, 30}},
		},
		"2 temporal layers": {
			startTs:          12345678,
			startFrameNumber: 100,
			fps:              [][]float32{{7.5, 15}, {7.5, 15}, {15, 30}},
		},
	}

	for name, c := range cases {
		testCase := c
		t.Run(name, func(t *testing.T) {
			fps := testCase.fps
			frames := make([][]*testFrameInfo, 0)
			h26xcalcs := make([]*FrameRateCalculatorH26x, len(fps))
			for i := range h26xcalcs {
				h26xcalcs[i] = NewFrameRateCalculatorH26x(90000, logger.GetLogger())
				frames = append(frames, createFrames(c.startFrameNumber, c.startTs, c.startSeq, 200, [][]float32{fps[i]}, false)[0])
			}

			var frameratesGot bool
			for s, fs := range frames {
				for _, f := range fs {
					if h26xcalcs[s].RecvPacket(f.toH26x()) {
						frameratesGot = true
						for _, calc := range h26xcalcs {
							if !calc.Completed() {
								frameratesGot = false
								break
							}
						}
					}
				}
			}
			require.True(t, frameratesGot)
			for i, calc := range h26xcalcs {
				fpsExpected := fps[i]
				fpsGot := calc.GetFrameRate()
				verifyFps(t, fpsExpected, fpsGot[:len(fpsExpected)])
			}
		})
	}

	t.Run("packet lost and duplicate", func(t *testing.T) {
		fps := [][]float32{{7.5, 15, 30}, {7.5, 15, 30}, {7.5, 15, 30}}
		frames := make([][]*testFrameInfo, 0, len(fps))
		h26xcalcs := make([]FrameRateCalculator, len(fps))
		for i := range fps {
			frames = append(frames, createFrames(100, 12345678, 10, 500, [][]float32{fps[i]}, false)[0])
			h26xcalcs[i] = NewFrameRateCalculatorH26x(90000, logger.GetLogger())
			for j := 5; j < 130; j++ {
				if j%2 == 0 {
					frames[i][j] = frames[i][j-1]
				}
			}
			for j := 130; j < 230; j++ {
				if j%3 == 0 {
					frames[i][j] = nil
				}
			}
			for j := 230; j < 330; j++ {
				if j%2 == 0 {
					frames[i][j], frames[i][j-1] = frames[i][j-1], frames[i][j]
				}
			}
		}
		var frameratesGot bool
		for s, fs := range frames {
			for _, f := range fs {
				if f == nil {
					continue
				}
				if h26xcalcs[s].RecvPacket(f.toH26x()) {
					frameratesGot = true
					for _, calc := range h26xcalcs {
						if !calc.Completed() {
							frameratesGot = false
							break
						}
					}
				}
			}
		}
		require.True(t, frameratesGot)
		for i, calc := range h26xcalcs {
			fpsExpected := fps[i]
			fpsGot := calc.GetFrameRate()
			verifyFps(t, fpsExpected, fpsGot[:len(fpsExpected)])
		}
	})
}

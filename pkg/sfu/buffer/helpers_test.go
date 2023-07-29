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

	"github.com/stretchr/testify/require"
)

func TestVP8Helper_Unmarshal(t *testing.T) {
	type args struct {
		payload []byte
	}
	tests := []struct {
		name            string
		args            args
		wantErr         bool
		checkTemporal   bool
		temporalSupport bool
		checkKeyFrame   bool
		keyFrame        bool
		checkPictureID  bool
		pictureID       uint16
		checkTlzIdx     bool
		tlzIdx          uint8
		checkTempID     bool
		temporalID      uint8
	}{
		{
			name:    "Empty or nil payload must return error",
			args:    args{payload: []byte{}},
			wantErr: true,
		},
		{
			name:            "Temporal must be supported by setting T bit to 1",
			args:            args{payload: []byte{0xff, 0x20, 0x1, 0x2, 0x3, 0x4}},
			checkTemporal:   true,
			temporalSupport: true,
		},
		{
			name:           "Picture must be ID 7 bits by setting M bit to 0 and present by I bit set to 1",
			args:           args{payload: []byte{0xff, 0xff, 0x11, 0x2, 0x3, 0x4}},
			checkPictureID: true,
			pictureID:      17,
		},
		{
			name:           "Picture ID must be 15 bits by setting M bit to 1 and present by I bit set to 1",
			args:           args{payload: []byte{0xff, 0xff, 0x92, 0x67, 0x3, 0x4, 0x5}},
			checkPictureID: true,
			pictureID:      4711,
		},
		{
			name:        "Temporal level zero index must be present if L set to 1",
			args:        args{payload: []byte{0xff, 0xff, 0xff, 0xfd, 0xb4, 0x4, 0x5}},
			checkTlzIdx: true,
			tlzIdx:      180,
		},
		{
			name:        "Temporal index must be present and used if T bit set to 1",
			args:        args{payload: []byte{0xff, 0xff, 0xff, 0xfd, 0xb4, 0x9f, 0x5, 0x6}},
			checkTempID: true,
			temporalID:  2,
		},
		{
			name:          "Check if packet is a keyframe by looking at P bit set to 0",
			args:          args{payload: []byte{0xff, 0xff, 0xff, 0xfd, 0xb4, 0x9f, 0x94, 0x1}},
			checkKeyFrame: true,
			keyFrame:      true,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			p := &VP8{}
			if err := p.Unmarshal(tt.args.payload); (err != nil) != tt.wantErr {
				t.Errorf("Unmarshal() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.checkTemporal {
				require.Equal(t, tt.temporalSupport, p.T)
			}
			if tt.checkKeyFrame {
				require.Equal(t, tt.keyFrame, p.IsKeyFrame)
			}
			if tt.checkPictureID {
				require.Equal(t, tt.pictureID, p.PictureID)
			}
			if tt.checkTlzIdx {
				require.Equal(t, tt.tlzIdx, p.TL0PICIDX)
			}
			if tt.checkTempID {
				require.Equal(t, tt.temporalID, p.TID)
			}
		})
	}
}

// ------------------------------------------

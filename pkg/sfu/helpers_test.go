package sfu

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
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
			name:    "Small payloads must return errors",
			args:    args{payload: []byte{0x0, 0x1, 0x2}},
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
			p := &VP8Helper{}
			if err := p.Unmarshal(tt.args.payload); (err != nil) != tt.wantErr {
				t.Errorf("Unmarshal() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.checkTemporal {
				assert.Equal(t, tt.temporalSupport, p.TemporalSupported)
			}
			if tt.checkKeyFrame {
				assert.Equal(t, tt.keyFrame, p.IsKeyFrame)
			}
			if tt.checkPictureID {
				assert.Equal(t, tt.pictureID, p.PictureID)
			}
			if tt.checkTlzIdx {
				assert.Equal(t, tt.tlzIdx, p.TL0PICIDX)
			}
			if tt.checkTempID {
				assert.Equal(t, tt.temporalID, p.TID)
			}
		})
	}
}

func Test_timeToNtp(t *testing.T) {
	type args struct {
		ns int64
	}
	tests := []struct {
		name    string
		args    args
		wantNTP uint64
	}{
		{
			name: "Must return correct NTP time",
			args: args{
				ns: time.Unix(1602391458, 1234).UnixNano(),
			},
			wantNTP: 16369753560730047667,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			gotNTP := timeToNtp(tt.args.ns)
			if gotNTP != tt.wantNTP {
				t.Errorf("timeToNtp() gotFraction = %v, want %v", gotNTP, tt.wantNTP)
			}
			println(gotNTP >> 16)
		})
	}
}

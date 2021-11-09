package sfu

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
)

func Test_audioLevel_addStream(t *testing.T) {
	type args struct {
		streamID string
	}
	tests := []struct {
		name string
		args args
		want audioStream
	}{
		{
			name: "Must add stream to audio level monitor",
			args: args{
				streamID: "a",
			},
			want: audioStream{
				id: "a",
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			a := &AudioObserver{}
			a.addStream(tt.args.streamID)
			assert.Equal(t, tt.want, *a.streams[0])

		})
	}
}

func Test_audioLevel_calc(t *testing.T) {
	type fields struct {
		streams  []*audioStream
		expected int
		previous []string
	}
	tests := []struct {
		name   string
		fields fields
		want   []string
	}{
		{
			name: "Must return streams that are above filter",
			fields: fields{
				streams: []*audioStream{
					{
						id:    "a",
						sum:   1,
						total: 5,
					},
					{
						id:    "b",
						sum:   2,
						total: 5,
					},
					{
						id:    "c",
						sum:   2,
						total: 2,
					},
				},
				expected: 3,
			},
			want: []string{"a", "b"},
		},
		{
			name: "Must return nil if result is same as previous",
			fields: fields{
				streams: []*audioStream{
					{
						id:    "a",
						sum:   1,
						total: 5,
					},
					{
						id:    "b",
						sum:   2,
						total: 5,
					},
					{
						id:    "c",
						sum:   2,
						total: 2,
					},
				},
				expected: 3,
				previous: []string{"a", "b"},
			},
			want: nil,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			a := &AudioObserver{
				streams:  tt.fields.streams,
				expected: tt.fields.expected,
				previous: tt.fields.previous,
			}
			if got := a.Calc(); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Calc() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_audioLevel_observe(t *testing.T) {
	type fields struct {
		streams   []*audioStream
		threshold uint8
	}
	type args struct {
		streamID string
		dBov     uint8
	}
	tests := []struct {
		name   string
		fields fields
		args   args
		want   audioStream
	}{
		{
			name: "Must increase sum and total when dBov is above threshold",
			fields: fields{
				streams: []*audioStream{
					{
						id:    "a",
						sum:   0,
						total: 0,
					},
				},
				threshold: 40,
			},
			args: args{
				streamID: "a",
				dBov:     20,
			},
			want: audioStream{
				id:    "a",
				sum:   20,
				total: 1,
			},
		},
		{
			name: "Must not increase sum and total when dBov is below threshold",
			fields: fields{
				streams: []*audioStream{
					{
						id:    "a",
						sum:   0,
						total: 0,
					},
				},
				threshold: 40,
			},
			args: args{
				streamID: "a",
				dBov:     60,
			},
			want: audioStream{
				id:    "a",
				sum:   0,
				total: 0,
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			a := &AudioObserver{
				streams:   tt.fields.streams,
				threshold: tt.fields.threshold,
			}
			a.observe(tt.args.streamID, tt.args.dBov)
			assert.Equal(t, *a.streams[0], tt.want)
		})
	}
}

func Test_audioLevel_removeStream(t *testing.T) {
	type fields struct {
		streams []*audioStream
	}
	type args struct {
		streamID string
	}
	tests := []struct {
		name   string
		fields fields
		args   args
	}{
		{
			name: "Must remove correct ID",
			fields: fields{
				streams: []*audioStream{
					{
						id: "a",
					},
					{
						id: "b",
					},
					{
						id: "c",
					},
					{
						id: "d",
					},
				},
			},
			args: args{
				streamID: "b",
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			a := &AudioObserver{
				streams: tt.fields.streams,
			}
			a.removeStream(tt.args.streamID)
			assert.Equal(t, len(a.streams), len(tt.fields.streams)-1)
			for _, s := range a.streams {
				assert.NotEqual(t, s.id, tt.args.streamID)
			}
		})
	}
}

func Test_newAudioLevel(t *testing.T) {
	type args struct {
		threshold uint8
		interval  int
		filter    int
	}
	tests := []struct {
		name string
		args args
		want *AudioObserver
	}{
		{
			name: "Must return a new audio level",
			args: args{
				threshold: 40,
				interval:  1000,
				filter:    20,
			},
			want: &AudioObserver{
				expected:  1000 * 20 / 2000,
				threshold: 40,
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			if got := NewAudioObserver(tt.args.threshold, tt.args.interval, tt.args.filter); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("NewAudioLevel() = %v, want %v", got, tt.want)
			}
		})
	}
}

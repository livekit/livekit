package sfu

import (
	"testing"
	"time"
)

func Test_timeToNtp(t *testing.T) {
	type args struct {
		ns time.Time
	}
	tests := []struct {
		name    string
		args    args
		wantNTP uint64
	}{
		{
			name: "Must return correct NTP time",
			args: args{
				ns: time.Unix(1602391458, 1234),
			},
			wantNTP: 16369753560730047668,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			gotNTP := uint64(toNtpTime(tt.args.ns))
			if gotNTP != tt.wantNTP {
				t.Errorf("timeToNtp() gotFraction = %v, want %v", gotNTP, tt.wantNTP)
			}
		})
	}
}

package buffer

import (
	"testing"
	"time"
)

func TestUplinkImpairLossWindow(t *testing.T) {
	started := time.Unix(100, 0)
	impair := &uplinkImpair{
		burst:   100 * time.Millisecond,
		gap:     200 * time.Millisecond,
		started: started,
	}

	tests := []struct {
		name string
		at   time.Duration
		want bool
	}{
		{name: "start in burst", at: 0, want: true},
		{name: "inside burst", at: 99 * time.Millisecond, want: true},
		{name: "gap begins", at: 100 * time.Millisecond, want: false},
		{name: "inside gap", at: 250 * time.Millisecond, want: false},
		{name: "next burst", at: 300 * time.Millisecond, want: true},
		{name: "next gap", at: 450 * time.Millisecond, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := impair.inLossWindow(started.Add(tt.at)); got != tt.want {
				t.Fatalf("inLossWindow() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestUplinkImpairLossWindowWithoutBurst(t *testing.T) {
	impair := &uplinkImpair{started: time.Unix(100, 0)}

	if !impair.inLossWindow(time.Unix(200, 0)) {
		t.Fatal("inLossWindow() = false, want true without burst config")
	}
}

func TestUplinkImpairLossWindowWithoutGap(t *testing.T) {
	started := time.Unix(100, 0)
	impair := &uplinkImpair{burst: 100 * time.Millisecond, started: started}

	if !impair.inLossWindow(started.Add(time.Hour)) {
		t.Fatal("inLossWindow() = false, want true when burst is set without gap")
	}
}

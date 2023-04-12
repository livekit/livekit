package utils

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWrapAroundUint16(t *testing.T) {
	w := NewWrapAround[uint16, uint32]()
	testCases := []struct {
		name            string
		input           uint16
		updated         wrapAroundUpdateResult[uint32]
		start           uint16
		extendedStart   uint32
		highest         uint16
		extendedHighest uint32
	}{
		// initialize
		{
			name:  "initialize",
			input: 10,
			updated: wrapAroundUpdateResult[uint32]{
				IsRestart:          false,
				PreExtendedStart:   0,
				PreExtendedHighest: 9,
				ExtendedVal:        10,
			},
			start:           10,
			extendedStart:   10,
			highest:         10,
			extendedHighest: 10,
		},
		// an older number without wrap around should reset start point
		{
			name:  "reset start no wrap around",
			input: 8,
			updated: wrapAroundUpdateResult[uint32]{
				IsRestart:          true,
				PreExtendedStart:   10,
				PreExtendedHighest: 10,
				ExtendedVal:        8,
			},
			start:           8,
			extendedStart:   8,
			highest:         10,
			extendedHighest: 10,
		},
		// an older number with wrap around should reset start point
		{
			name:  "reset start wrap around",
			input: (1 << 16) - 6,
			updated: wrapAroundUpdateResult[uint32]{
				IsRestart:          true,
				PreExtendedStart:   8,
				PreExtendedHighest: 10,
				ExtendedVal:        (1 << 16) - 6,
			},
			start:           (1 << 16) - 6,
			extendedStart:   (1 << 16) - 6,
			highest:         10,
			extendedHighest: (1 << 16) + 10,
		},
		// an older number with wrap around should reset start point again
		{
			name:  "reset start again",
			input: (1 << 16) - 12,
			updated: wrapAroundUpdateResult[uint32]{
				IsRestart:          true,
				PreExtendedStart:   (1 << 16) - 6,
				PreExtendedHighest: (1 << 16) + 10,
				ExtendedVal:        (1 << 16) - 12,
			},
			start:           (1 << 16) - 12,
			extendedStart:   (1 << 16) - 12,
			highest:         10,
			extendedHighest: (1 << 16) + 10,
		},
		// duplicate should return same as highest
		{
			name:  "duplicate",
			input: 10,
			updated: wrapAroundUpdateResult[uint32]{
				IsRestart:          false,
				PreExtendedStart:   0,
				PreExtendedHighest: (1 << 16) + 10,
				ExtendedVal:        (1 << 16) + 10,
			},
			start:           (1 << 16) - 12,
			extendedStart:   (1 << 16) - 12,
			highest:         10,
			extendedHighest: (1 << 16) + 10,
		},
		// a significant jump in order should not reset start
		{
			name:  "big in-order jump",
			input: 1 << 15,
			updated: wrapAroundUpdateResult[uint32]{
				IsRestart:          false,
				PreExtendedStart:   0,
				PreExtendedHighest: (1 << 16) + 10,
				ExtendedVal:        (1 << 16) + (1 << 15),
			},
			start:           (1 << 16) - 12,
			extendedStart:   (1 << 16) - 12,
			highest:         1 << 15,
			extendedHighest: (1 << 16) + (1 << 15),
		},
		// now out-of-order should not reset start as half the range has been seen
		{
			name:  "out-of-order after half range",
			input: (1 << 15) - 1,
			updated: wrapAroundUpdateResult[uint32]{
				IsRestart:          false,
				PreExtendedStart:   0,
				PreExtendedHighest: (1 << 16) + (1 << 15),
				ExtendedVal:        (1 << 16) + (1 << 15) - 1,
			},
			start:           (1 << 16) - 12,
			extendedStart:   (1 << 16) - 12,
			highest:         1 << 15,
			extendedHighest: (1 << 16) + (1 << 15),
		},
		// in-order, should update highest
		{
			name:  "in-order",
			input: (1 << 15) + 3,
			updated: wrapAroundUpdateResult[uint32]{
				IsRestart:          false,
				PreExtendedStart:   0,
				PreExtendedHighest: (1 << 16) + (1 << 15),
				ExtendedVal:        (1 << 16) + (1 << 15) + 3,
			},
			start:           (1 << 16) - 12,
			extendedStart:   (1 << 16) - 12,
			highest:         (1 << 15) + 3,
			extendedHighest: (1 << 16) + (1 << 15) + 3,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.updated, w.Update(tc.input))
			require.Equal(t, tc.start, w.GetStart())
			require.Equal(t, tc.extendedStart, w.GetExtendedStart())
			require.Equal(t, tc.highest, w.GetHighest())
			require.Equal(t, tc.extendedHighest, w.GetExtendedHighest())
		})
	}
}

func TestWrapAroundUint32(t *testing.T) {
	w := NewWrapAround[uint32, uint64]()
	testCases := []struct {
		name            string
		input           uint32
		updated         wrapAroundUpdateResult[uint64]
		start           uint32
		extendedStart   uint64
		highest         uint32
		extendedHighest uint64
	}{
		// initialize
		{
			name:  "initialize",
			input: 10,
			updated: wrapAroundUpdateResult[uint64]{
				IsRestart:          false,
				PreExtendedStart:   0,
				PreExtendedHighest: 9,
				ExtendedVal:        10,
			},
			start:           10,
			extendedStart:   10,
			highest:         10,
			extendedHighest: 10,
		},
		// an older number without wrap around should reset start point
		{
			name:  "reset start no wrap around",
			input: 8,
			updated: wrapAroundUpdateResult[uint64]{
				IsRestart:          true,
				PreExtendedStart:   10,
				PreExtendedHighest: 10,
				ExtendedVal:        8,
			},
			start:           8,
			extendedStart:   8,
			highest:         10,
			extendedHighest: 10,
		},
		// an older number with wrap around should reset start point
		{
			name:  "reset start wrap around",
			input: (1 << 32) - 6,
			updated: wrapAroundUpdateResult[uint64]{
				IsRestart:          true,
				PreExtendedStart:   8,
				PreExtendedHighest: 10,
				ExtendedVal:        (1 << 32) - 6,
			},
			start:           (1 << 32) - 6,
			extendedStart:   (1 << 32) - 6,
			highest:         10,
			extendedHighest: (1 << 32) + 10,
		},
		// an older number with wrap around should reset start point again
		{
			name:  "reset start again",
			input: (1 << 32) - 12,
			updated: wrapAroundUpdateResult[uint64]{
				IsRestart:          true,
				PreExtendedStart:   (1 << 32) - 6,
				PreExtendedHighest: (1 << 32) + 10,
				ExtendedVal:        (1 << 32) - 12,
			},
			start:           (1 << 32) - 12,
			extendedStart:   (1 << 32) - 12,
			highest:         10,
			extendedHighest: (1 << 32) + 10,
		},
		// duplicate should return same as highest
		{
			name:  "duplicate",
			input: 10,
			updated: wrapAroundUpdateResult[uint64]{
				IsRestart:          false,
				PreExtendedStart:   0,
				PreExtendedHighest: (1 << 32) + 10,
				ExtendedVal:        (1 << 32) + 10,
			},
			start:           (1 << 32) - 12,
			extendedStart:   (1 << 32) - 12,
			highest:         10,
			extendedHighest: (1 << 32) + 10,
		},
		// a significant jump in order should not reset start
		{
			name:  "big in-order jump",
			input: 1 << 31,
			updated: wrapAroundUpdateResult[uint64]{
				IsRestart:          false,
				PreExtendedStart:   0,
				PreExtendedHighest: (1 << 32) + 10,
				ExtendedVal:        (1 << 32) + (1 << 31),
			},
			start:           (1 << 32) - 12,
			extendedStart:   (1 << 32) - 12,
			highest:         1 << 31,
			extendedHighest: (1 << 32) + (1 << 31),
		},
		// now out-of-order should not reset start as half the range has been seen
		{
			name:  "out-of-order after half range",
			input: (1 << 31) - 1,
			updated: wrapAroundUpdateResult[uint64]{
				IsRestart:          false,
				PreExtendedStart:   0,
				PreExtendedHighest: (1 << 32) + (1 << 31),
				ExtendedVal:        (1 << 32) + (1 << 31) - 1,
			},
			start:           (1 << 32) - 12,
			extendedStart:   (1 << 32) - 12,
			highest:         1 << 31,
			extendedHighest: (1 << 32) + (1 << 31),
		},
		// in-order, should update highest
		{
			name:  "in-order",
			input: (1 << 31) + 3,
			updated: wrapAroundUpdateResult[uint64]{
				IsRestart:          false,
				PreExtendedStart:   0,
				PreExtendedHighest: (1 << 32) + (1 << 31),
				ExtendedVal:        (1 << 32) + (1 << 31) + 3,
			},
			start:           (1 << 32) - 12,
			extendedStart:   (1 << 32) - 12,
			highest:         (1 << 31) + 3,
			extendedHighest: (1 << 32) + (1 << 31) + 3,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.updated, w.Update(tc.input))
			require.Equal(t, tc.start, w.GetStart())
			require.Equal(t, tc.extendedStart, w.GetExtendedStart())
			require.Equal(t, tc.highest, w.GetHighest())
			require.Equal(t, tc.extendedHighest, w.GetExtendedHighest())
		})
	}
}

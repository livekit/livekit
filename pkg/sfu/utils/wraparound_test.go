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

package utils

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWrapAroundUint16(t *testing.T) {
	w := NewWrapAround[uint16, uint32](WrapAroundParams{IsRestartAllowed: true})
	testCases := []struct {
		name            string
		input           uint16
		updated         WrapAroundUpdateResult[uint32]
		start           uint16
		extendedStart   uint32
		highest         uint16
		extendedHighest uint32
	}{
		// initialize
		{
			name:  "initialize",
			input: 10,
			updated: WrapAroundUpdateResult[uint32]{
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
			updated: WrapAroundUpdateResult[uint32]{
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
			updated: WrapAroundUpdateResult[uint32]{
				IsRestart:          true,
				PreExtendedStart:   (1 << 16) + 8,
				PreExtendedHighest: (1 << 16) + 10,
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
			updated: WrapAroundUpdateResult[uint32]{
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
		// out of order with highest, wrap back, but no restart
		{
			name:  "out of order - no restart",
			input: (1 << 16) - 3,
			updated: WrapAroundUpdateResult[uint32]{
				IsRestart:          false,
				PreExtendedStart:   0,
				PreExtendedHighest: (1 << 16) + 10,
				ExtendedVal:        (1 << 16) - 3,
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
			updated: WrapAroundUpdateResult[uint32]{
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
			input: (1 << 15) - 10,
			updated: WrapAroundUpdateResult[uint32]{
				IsRestart:          false,
				PreExtendedStart:   0,
				PreExtendedHighest: (1 << 16) + 10,
				ExtendedVal:        (1 << 16) + (1 << 15) - 10,
			},
			start:           (1 << 16) - 12,
			extendedStart:   (1 << 16) - 12,
			highest:         (1 << 15) - 10,
			extendedHighest: (1 << 16) + (1 << 15) - 10,
		},
		// now out-of-order should not reset start as half the range has been seen
		{
			name:  "out-of-order after half range",
			input: (1 << 15) - 11,
			updated: WrapAroundUpdateResult[uint32]{
				IsRestart:          false,
				PreExtendedStart:   0,
				PreExtendedHighest: (1 << 16) + (1 << 15) - 10,
				ExtendedVal:        (1 << 16) + (1 << 15) - 11,
			},
			start:           (1 << 16) - 12,
			extendedStart:   (1 << 16) - 12,
			highest:         (1 << 15) - 10,
			extendedHighest: (1 << 16) + (1 << 15) - 10,
		},
		// wrap back out-of-order
		{
			name:  "wrap back out-of-order after half range",
			input: (1 << 16) - 1,
			updated: WrapAroundUpdateResult[uint32]{
				IsRestart:          false,
				PreExtendedStart:   0,
				PreExtendedHighest: (1 << 16) + (1 << 15) - 10,
				ExtendedVal:        (1 << 16) - 1,
			},
			start:           (1 << 16) - 12,
			extendedStart:   (1 << 16) - 12,
			highest:         (1 << 15) - 10,
			extendedHighest: (1 << 16) + (1 << 15) - 10,
		},
		// in-order, should update highest
		{
			name:  "in-order",
			input: (1 << 15) + 3,
			updated: WrapAroundUpdateResult[uint32]{
				IsRestart:          false,
				PreExtendedStart:   0,
				PreExtendedHighest: (1 << 16) + (1 << 15) - 10,
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

func TestWrapAroundUint16NoRestart(t *testing.T) {
	w := NewWrapAround[uint16, uint32](WrapAroundParams{IsRestartAllowed: false})
	testCases := []struct {
		name            string
		input           uint16
		updated         WrapAroundUpdateResult[uint32]
		start           uint16
		extendedStart   uint32
		highest         uint16
		extendedHighest uint32
	}{
		// initialize
		{
			name:  "initialize",
			input: 10,
			updated: WrapAroundUpdateResult[uint32]{
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
		// an older number without wrap around should not reset start point
		{
			name:  "no reset start no wrap around",
			input: 8,
			updated: WrapAroundUpdateResult[uint32]{
				IsUnhandled: true,
				// the following fields are not valid when `IsUnhandled = true`, but code fills it in
				// and they are filled in here for testing purposes
				PreExtendedHighest: 10,
				ExtendedVal:        8,
			},
			start:           10,
			extendedStart:   10,
			highest:         10,
			extendedHighest: 10,
		},
		// an older number with wrap around should not reset start point
		{
			name:  "no reset start wrap around",
			input: (1 << 16) - 6,
			updated: WrapAroundUpdateResult[uint32]{
				IsUnhandled:        true,
				PreExtendedHighest: 10,
				ExtendedVal:        (1 << 16) - 6,
			},
			start:           10,
			extendedStart:   10,
			highest:         10,
			extendedHighest: 10,
		},
		// yet another older number with wrap around should not reset start point
		{
			name:  "no reset start again",
			input: (1 << 16) - 12,
			updated: WrapAroundUpdateResult[uint32]{
				IsUnhandled:        true,
				PreExtendedHighest: 10,
				ExtendedVal:        (1 << 16) - 12,
			},
			start:           10,
			extendedStart:   10,
			highest:         10,
			extendedHighest: 10,
		},
		// duplicate should return same as highest
		{
			name:  "duplicate",
			input: 10,
			updated: WrapAroundUpdateResult[uint32]{
				PreExtendedHighest: 10,
				ExtendedVal:        10,
			},
			start:           10,
			extendedStart:   10,
			highest:         10,
			extendedHighest: 10,
		},
		// a significant jump in order should move highest to that
		{
			name:  "big in-order jump",
			input: (1 << 15) - 10,
			updated: WrapAroundUpdateResult[uint32]{
				PreExtendedHighest: 10,
				ExtendedVal:        (1 << 15) - 10,
			},
			start:           10,
			extendedStart:   10,
			highest:         (1 << 15) - 10,
			extendedHighest: (1 << 15) - 10,
		},
		// in-order, should update highest
		{
			name:  "in-order",
			input: (1 << 15) + 13,
			updated: WrapAroundUpdateResult[uint32]{
				PreExtendedHighest: (1 << 15) - 10,
				ExtendedVal:        (1 << 15) + 13,
			},
			start:           10,
			extendedStart:   10,
			highest:         (1 << 15) + 13,
			extendedHighest: (1 << 15) + 13,
		},
		// now out-of-order should not reset start as half the range has been seen
		{
			name:  "out-of-order after half range",
			input: (1 << 15) - 11,
			updated: WrapAroundUpdateResult[uint32]{
				PreExtendedHighest: (1 << 15) + 13,
				ExtendedVal:        (1 << 15) - 11,
			},
			start:           10,
			extendedStart:   10,
			highest:         (1 << 15) + 13,
			extendedHighest: (1 << 15) + 13,
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

func TestWrapAroundUint16RollbackRestartAndResetHighest(t *testing.T) {
	w := NewWrapAround[uint16, uint64](WrapAroundParams{IsRestartAllowed: true})

	// initialize
	w.Update(23)
	require.Equal(t, uint16(23), w.GetStart())
	require.Equal(t, uint64(23), w.GetExtendedStart())
	require.Equal(t, uint16(23), w.GetHighest())
	require.Equal(t, uint64(23), w.GetExtendedHighest())

	// an in-order update
	w.Update(25)
	require.Equal(t, uint16(23), w.GetStart())
	require.Equal(t, uint64(23), w.GetExtendedStart())
	require.Equal(t, uint16(25), w.GetHighest())
	require.Equal(t, uint64(25), w.GetExtendedHighest())

	// force restart without wrap
	res := w.Update(12)
	expectedResult := WrapAroundUpdateResult[uint64]{
		IsRestart:          true,
		PreExtendedStart:   23,
		PreExtendedHighest: 25,
		ExtendedVal:        12,
	}
	require.Equal(t, expectedResult, res)
	require.Equal(t, uint16(12), w.GetStart())
	require.Equal(t, uint64(12), w.GetExtendedStart())
	require.Equal(t, uint16(25), w.GetHighest())
	require.Equal(t, uint64(25), w.GetExtendedHighest())

	// roll back restart
	w.RollbackRestart(res.PreExtendedStart)
	require.Equal(t, uint16(23), w.GetStart())
	require.Equal(t, uint64(23), w.GetExtendedStart())
	require.Equal(t, uint16(25), w.GetHighest())
	require.Equal(t, uint64(25), w.GetExtendedHighest())

	// force restart with wrap
	res = w.Update(65533)
	expectedResult = WrapAroundUpdateResult[uint64]{
		IsRestart:          true,
		PreExtendedStart:   (1 << 16) + 23,
		PreExtendedHighest: (1 << 16) + 25,
		ExtendedVal:        65533,
	}
	require.Equal(t, expectedResult, res)
	require.Equal(t, uint16(65533), w.GetStart())
	require.Equal(t, uint64(65533), w.GetExtendedStart())
	require.Equal(t, uint16(25), w.GetHighest())
	require.Equal(t, uint64(65536+25), w.GetExtendedHighest())

	// roll back restart
	w.RollbackRestart(res.PreExtendedStart)
	require.Equal(t, uint16(23), w.GetStart())
	require.Equal(t, uint64(23), w.GetExtendedStart())
	require.Equal(t, uint16(25), w.GetHighest())
	require.Equal(t, uint64(25), w.GetExtendedHighest())

	// reset highest
	w.ResetHighest(0x1234)
	require.Equal(t, uint16(23), w.GetStart())
	require.Equal(t, uint64(23), w.GetExtendedStart())
	require.Equal(t, uint16(0x1234), w.GetHighest())
	require.Equal(t, uint64(0x1234), w.GetExtendedHighest())

	w.ResetHighest(0x7f1234)
	require.Equal(t, uint16(23), w.GetStart())
	require.Equal(t, uint64(23), w.GetExtendedStart())
	require.Equal(t, uint16(0x1234), w.GetHighest())
	require.Equal(t, uint64(0x7f1234), w.GetExtendedHighest())
}

func TestWrapAroundUint16WrapAroundRestartDuplicate(t *testing.T) {
	w := NewWrapAround[uint16, uint64](WrapAroundParams{IsRestartAllowed: true})

	// initialize
	w.Update(65534)
	require.Equal(t, uint16(65534), w.GetStart())
	require.Equal(t, uint64(65534), w.GetExtendedStart())
	require.Equal(t, uint16(65534), w.GetHighest())
	require.Equal(t, uint64(65534), w.GetExtendedHighest())

	// an in-order update with a roll over
	w.Update(32)
	require.Equal(t, uint16(65534), w.GetStart())
	require.Equal(t, uint64(65534), w.GetExtendedStart())
	require.Equal(t, uint16(32), w.GetHighest())
	require.Equal(t, uint64(65568), w.GetExtendedHighest())

	// duplicate of start
	res := w.Update(65534)
	expectedResult := WrapAroundUpdateResult[uint64]{
		IsRestart:          false,
		PreExtendedStart:   0,
		PreExtendedHighest: 65568,
		ExtendedVal:        65534,
	}
	require.Equal(t, expectedResult, res)
	require.Equal(t, uint16(65534), w.GetStart())
	require.Equal(t, uint64(65534), w.GetExtendedStart())
	require.Equal(t, uint16(32), w.GetHighest())
	require.Equal(t, uint64(65568), w.GetExtendedHighest())

	// duplicate of start - again
	res = w.Update(65534)
	expectedResult = WrapAroundUpdateResult[uint64]{
		IsRestart:          false,
		PreExtendedStart:   0,
		PreExtendedHighest: 65568,
		ExtendedVal:        65534,
	}
	require.Equal(t, expectedResult, res)
	require.Equal(t, uint16(65534), w.GetStart())
	require.Equal(t, uint64(65534), w.GetExtendedStart())
	require.Equal(t, uint16(32), w.GetHighest())
	require.Equal(t, uint64(65568), w.GetExtendedHighest())
}

func TestWrapAroundUint16Rollover(t *testing.T) {
	w := NewWrapAround[uint16, uint32](WrapAroundParams{IsRestartAllowed: false})
	testCases := []struct {
		name            string
		input           uint16
		numCycles       int
		updated         WrapAroundUpdateResult[uint32]
		start           uint16
		extendedStart   uint32
		highest         uint16
		extendedHighest uint32
	}{
		// initialize - should initialize irrespective of numCycles
		{
			name:      "initialize",
			input:     10,
			numCycles: 10,
			updated: WrapAroundUpdateResult[uint32]{
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
		// negative cycles - should just do an update
		{
			name:      "zero",
			input:     8,
			numCycles: -1,
			updated: WrapAroundUpdateResult[uint32]{
				IsUnhandled: true,
				// the following fields are not valid when `IsUnhandled = true`, but code fills it in
				// and they are filled in here for testing purposes
				PreExtendedHighest: 10,
				ExtendedVal:        8,
			},
			start:           10,
			extendedStart:   10,
			highest:         10,
			extendedHighest: 10,
		},
		// one cycle
		{
			name:      "one cycle",
			input:     (1 << 16) - 6,
			numCycles: 1,
			updated: WrapAroundUpdateResult[uint32]{
				PreExtendedHighest: 10,
				ExtendedVal:        (1 << 16) - 6 + (1 << 16),
			},
			start:           10,
			extendedStart:   10,
			highest:         (1 << 16) - 6,
			extendedHighest: (1 << 16) - 6 + (1 << 16),
		},
		// two cycles
		{
			name:      "two cycles",
			input:     (1 << 16) - 7,
			numCycles: 2,
			updated: WrapAroundUpdateResult[uint32]{
				PreExtendedHighest: (1 << 16) - 6 + (1 << 16),
				ExtendedVal:        (1 << 16) - 7 + 3*(1<<16),
			},
			start:           10,
			extendedStart:   10,
			highest:         (1 << 16) - 7,
			extendedHighest: (1 << 16) - 7 + 3*(1<<16),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.updated, w.Rollover(tc.input, tc.numCycles))
			require.Equal(t, tc.start, w.GetStart())
			require.Equal(t, tc.extendedStart, w.GetExtendedStart())
			require.Equal(t, tc.highest, w.GetHighest())
			require.Equal(t, tc.extendedHighest, w.GetExtendedHighest())
		})
	}
}

func TestWrapAroundUint32(t *testing.T) {
	w := NewWrapAround[uint32, uint64](WrapAroundParams{IsRestartAllowed: true})
	testCases := []struct {
		name            string
		input           uint32
		updated         WrapAroundUpdateResult[uint64]
		start           uint32
		extendedStart   uint64
		highest         uint32
		extendedHighest uint64
	}{
		// initialize
		{
			name:  "initialize",
			input: 10,
			updated: WrapAroundUpdateResult[uint64]{
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
			updated: WrapAroundUpdateResult[uint64]{
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
			updated: WrapAroundUpdateResult[uint64]{
				IsRestart:          true,
				PreExtendedStart:   (1 << 32) + 8,
				PreExtendedHighest: (1 << 32) + 10,
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
			updated: WrapAroundUpdateResult[uint64]{
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
			updated: WrapAroundUpdateResult[uint64]{
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
			updated: WrapAroundUpdateResult[uint64]{
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
			updated: WrapAroundUpdateResult[uint64]{
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
			updated: WrapAroundUpdateResult[uint64]{
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

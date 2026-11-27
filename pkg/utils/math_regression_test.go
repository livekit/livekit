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

package utils

import (
	"math"
	"math/big"
	"math/rand"
	"testing"
)

func TestMedianAllInt8Pairs(t *testing.T) {
	for left := -128; left <= 127; left++ {
		for right := left; right <= 127; right++ {
			want := int8((left + right) / 2)
			if got := Median([]int8{int8(right), int8(left)}); got != want {
				t.Fatalf("Median(%d, %d) = %d, want %d", left, right, got, want)
			}
		}
	}
}

func TestMedianNumericLimits(t *testing.T) {
	if got := Median([]int64{math.MinInt64, math.MaxInt64}); got != 0 {
		t.Errorf("signed extremes: got %d, want 0", got)
	}
	if got := Median([]uint64{math.MaxUint64 - 2, math.MaxUint64}); got != math.MaxUint64-1 {
		t.Errorf("unsigned extremes: got %d, want %d", got, uint64(math.MaxUint64-1))
	}
	if got := Median([]float64{-math.MaxFloat64, math.MaxFloat64}); got != 0 {
		t.Errorf("opposite floating point extremes: got %g, want 0", got)
	}
	for _, inf := range []float64{math.Inf(-1), math.Inf(1)} {
		if got := Median([]float64{inf, inf}); got != inf {
			t.Errorf("identical infinities: got %g, want %g", got, inf)
		}
		if got := Median([]float64{-1, inf}); got != inf {
			t.Errorf("finite and infinite: got %g, want %g", got, inf)
		}
	}
}

func TestMedianSubnormalValues(t *testing.T) {
	small64 := math.SmallestNonzeroFloat64
	if got := Median([]float64{small64, 2 * small64}); got != 2*small64 {
		t.Errorf("positive float64 subnormals: got %g, want %g", got, 2*small64)
	}
	if got := Median([]float64{-2 * small64, -small64}); got != -2*small64 {
		t.Errorf("negative float64 subnormals: got %g, want %g", got, -2*small64)
	}
	small32 := float32(math.SmallestNonzeroFloat32)
	if got := Median([]float32{small32, 2 * small32}); got != 2*small32 {
		t.Errorf("positive float32 subnormals: got %g, want %g", got, 2*small32)
	}
	if got := Median([]float32{-2 * small32, -small32}); got != -2*small32 {
		t.Errorf("negative float32 subnormals: got %g, want %g", got, -2*small32)
	}
}

func TestMedianFloat64AgainstExactAverage(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for range 1024 {
		a, b := math.Float64frombits(rng.Uint64()), math.Float64frombits(rng.Uint64())
		if math.IsNaN(a) || math.IsNaN(b) || math.IsInf(a, 0) || math.IsInf(b, 0) {
			continue
		}
		exact := new(big.Rat).Add(new(big.Rat).SetFloat64(a), new(big.Rat).SetFloat64(b))
		exact.Quo(exact, big.NewRat(2, 1))
		want, _ := exact.Float64()
		if got := Median([]float64{a, b}); got != want {
			t.Fatalf("Median(%g, %g) = %g, want %g", a, b, got, want)
		}
	}
}

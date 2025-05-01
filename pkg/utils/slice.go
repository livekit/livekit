package utils

import (
	"cmp"
	"slices"
)

func DedupeSlice[T cmp.Ordered](s []T) []T {
	if len(s) < 2 {
		return s
	}

	slices.Sort(s)
	return slices.Compact(s)
}

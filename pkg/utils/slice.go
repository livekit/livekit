package utils

import (
	"cmp"
	"slices"
)

func DedupeSlice[T cmp.Ordered](s []T) []T {
	slices.Sort(s)
	return slices.Compact(s)
}

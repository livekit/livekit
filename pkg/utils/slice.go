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

	o := 1
	for i := 1; i < len(s); i++ {
		if s[i] == s[i-1] {
			continue
		}
		s[o] = s[i]
		o++
	}

	return s[:o]
}

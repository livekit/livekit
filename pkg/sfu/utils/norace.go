//go:build !race

package utils

// RaceEnabled reports whether the binary was built with the race detector.
const RaceEnabled = false

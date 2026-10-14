//go:build race

package utils

// RaceEnabled reports whether the binary was built with the race detector.
// sync.Pool drops a quarter of returned items under it, so allocation counts
// on pooled paths are not meaningful.
const RaceEnabled = true

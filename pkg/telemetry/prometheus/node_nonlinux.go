//go:build !linux

package prometheus

func getTCStats() (packets, drops uint32, err error) {
	// linux only
	return
}

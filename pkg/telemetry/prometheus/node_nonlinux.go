//go:build !linux
// +build !linux

package prometheus

func getSystemStats() (numCPUs uint32, avg1Min, avg5Min, avg15Min float32, err error) {
	return
}

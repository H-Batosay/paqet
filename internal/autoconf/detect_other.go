//go:build !linux && !darwin && !windows

package autoconf

import "fmt"

// Detect is not implemented on this platform.
func Detect(_ string) (*Info, error) {
	return nil, fmt.Errorf("network auto-detection is not supported on this platform; configure network settings manually")
}

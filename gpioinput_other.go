//go:build !linux

package main

import "fmt"

// OpenDigitalInputLines is only implemented on Linux (see
// gpioinput_linux.go) - the GPIO character device uAPI it uses is
// Linux-only, and it's what the CM5 board runs. This stub keeps the
// project buildable for local development on other platforms; the
// digital input module will simply report a fault, and simulation mode
// still works normally.
func OpenDigitalInputLines(chipPath string, offsets []int, consumer string) (DigitalInputLines, error) {
	return nil, fmt.Errorf("the GPIO character device is only available on Linux; cannot open %q on this platform", chipPath)
}

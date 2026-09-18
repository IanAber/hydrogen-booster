//go:build !linux

package main

import "fmt"

// OpenGpioLines is only implemented on Linux (see gpiochip_linux.go) -
// the GPIO character device uAPI it uses is Linux-only, and it's what
// the CM5 board runs. This stub keeps the project buildable for local
// development on other platforms; the relay module will simply report a
// fault, and simulation mode still works normally.
func OpenGpioLines(chipPath string, offsets []int, consumer string) (GpioLines, error) {
	return nil, fmt.Errorf("the GPIO character device is only available on Linux; cannot open %q on this platform", chipPath)
}

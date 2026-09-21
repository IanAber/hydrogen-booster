//go:build !linux

package main

import "fmt"

// OpenSerialPort is only implemented on Linux (see serialport_linux.go)
// - the termios ioctls it uses are Linux-specific. This stub keeps the
// project buildable for local development on other platforms; the flow
// module will simply report a connection error, and every other
// feature (including simulated fills) works normally.
func OpenSerialPort(path string, baud int) (SerialPort, error) {
	return nil, fmt.Errorf("serial ports are only available on Linux; cannot open %q on this platform", path)
}

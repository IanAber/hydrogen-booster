//go:build !linux

package main

import "fmt"

// OpenSocketCAN is only implemented on Linux (see cansocket_linux.go) -
// that's the only OS SocketCAN exists on, and it's what the CM5 board
// runs. This stub keeps the project buildable for local development on
// other platforms; the pressure module will simply report a comms
// fault, and simulation mode still works normally.
func OpenSocketCAN(ifaceName string) (CanTransport, error) {
	return nil, fmt.Errorf("SocketCAN is only available on Linux; cannot open %q on this platform", ifaceName)
}

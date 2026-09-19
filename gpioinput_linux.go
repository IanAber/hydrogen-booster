package main

import (
	"fmt"
	"syscall"
	"unsafe"
)

// This file talks to the Linux GPIO character device uAPI directly via
// ioctl, mirroring gpiochip_linux.go's approach but requesting lines as
// inputs instead of outputs. It's what exposes gpiochip0's lines for
// reading the hydraulic-oil-level and motor-powered feedback signals.
// The struct/ioctl-number definitions it uses live in gpioioctl.go
// (portable, so gpioioctl_test.go can check them without a Linux build).

// linuxDigitalInputLines is the real DigitalInputLines implementation:
// one open line request (a group of GPIO lines reserved together as
// inputs), backed by the file descriptor the kernel hands back from
// GPIO_V2_GET_LINE_IOCTL. GetValues addresses lines by their position
// within this request (0..n-1), not by chip offset.
type linuxDigitalInputLines struct {
	fd int
	n  int
}

// OpenDigitalInputLines requests offsets (gpiochip0 line numbers) on the
// gpiochip character device at chipPath as inputs, and returns a handle
// for reading them as a group. It's the real hardware driver behind
// DigitalInputModule (see gpioinput.go), called only once live data is
// switched on for at least one digital input channel from the admin
// screen.
func OpenDigitalInputLines(chipPath string, offsets []int, consumer string) (DigitalInputLines, error) {
	if len(offsets) == 0 {
		return nil, fmt.Errorf("no GPIO lines to request")
	}
	if len(offsets) > gpioV2LinesMax {
		return nil, fmt.Errorf("too many GPIO lines requested (%d > %d)", len(offsets), gpioV2LinesMax)
	}

	chipFd, err := syscall.Open(chipPath, syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", chipPath, err)
	}
	defer syscall.Close(chipFd)

	var info gpiochipInfo
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(chipFd), gpioGetChipInfoIoctl, uintptr(unsafe.Pointer(&info))); errno != 0 {
		return nil, fmt.Errorf("GPIO_GET_CHIPINFO_IOCTL on %s: %w", chipPath, errno)
	}
	for _, off := range offsets {
		if off < 0 || uint32(off) >= info.Lines {
			return nil, fmt.Errorf("line %d is out of range for %s (%s has %d lines)", off, chipPath, cString(info.Name[:]), info.Lines)
		}
	}

	var req gpioV2LineRequest
	for i, off := range offsets {
		req.Offsets[i] = uint32(off)
	}
	req.NumLines = uint32(len(offsets))
	copy(req.Consumer[:], consumer)
	req.Config.Flags = gpioV2LineFlagInput

	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(chipFd), gpioV2GetLineIoctl, uintptr(unsafe.Pointer(&req))); errno != 0 {
		return nil, fmt.Errorf("GPIO_V2_GET_LINE_IOCTL on %s (lines %v): %w", chipPath, offsets, errno)
	}

	return &linuxDigitalInputLines{fd: int(req.FD), n: len(offsets)}, nil
}

func (h *linuxDigitalInputLines) GetValues() (uint64, error) {
	v := gpioV2LineValues{Mask: lineMask(h.n)}
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(h.fd), gpioV2LineGetValuesIoctl, uintptr(unsafe.Pointer(&v))); errno != 0 {
		return 0, errno
	}
	return v.Bits, nil
}

func (h *linuxDigitalInputLines) Close() error {
	return syscall.Close(h.fd)
}

package main

import (
	"fmt"
	"syscall"
	"unsafe"
)

// This file talks to the Linux GPIO character device uAPI directly via
// ioctl - stdlib only (syscall, unsafe), no external GPIO library,
// matching cansocket_linux.go's approach to SocketCAN. It's what exposes
// the MCP23017 relay expander's 16 ports as /dev/gpiochip16's lines. The
// struct/ioctl-number definitions it uses live in gpioioctl.go (portable,
// so gpioioctl_test.go can check them without a Linux build).

// linuxGpioLines is the real GpioLines implementation: one open line
// request (a group of GPIO lines reserved together as outputs), backed
// by the file descriptor the kernel hands back from
// GPIO_V2_GET_LINE_IOCTL. SetValues/GetValues address lines by their
// position within this request (0..n-1), not by chip offset.
type linuxGpioLines struct {
	fd int
	n  int
}

// OpenGpioLines requests offsets (MCP23017 port numbers, 0-15) on the
// gpiochip character device at chipPath as outputs, initially all low
// (relays off - the safe default), and returns a handle for
// reading/writing them as a group. It's the real hardware driver behind
// RelayModule (see gpiochip.go), called only once live data is switched
// on for at least one relay channel from the admin screen.
func OpenGpioLines(chipPath string, offsets []int, consumer string) (GpioLines, error) {
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
			return nil, fmt.Errorf("port %d is out of range for %s (%s has %d lines)", off, chipPath, cString(info.Name[:]), info.Lines)
		}
	}

	var req gpioV2LineRequest
	for i, off := range offsets {
		req.Offsets[i] = uint32(off)
	}
	req.NumLines = uint32(len(offsets))
	copy(req.Consumer[:], consumer)
	req.Config.Flags = gpioV2LineFlagOutput

	// Explicitly request every line start low (relays de-energised) -
	// the safe default regardless of whatever level the pin happened to
	// be left at.
	req.Config.NumAttrs = 1
	req.Config.Attrs[0] = gpioV2LineConfigAttribute{
		Attr: gpioV2LineAttribute{ID: gpioV2LineAttrIDOutputValues, Value: 0},
		Mask: lineMask(len(offsets)),
	}

	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(chipFd), gpioV2GetLineIoctl, uintptr(unsafe.Pointer(&req))); errno != 0 {
		return nil, fmt.Errorf("GPIO_V2_GET_LINE_IOCTL on %s (ports %v): %w", chipPath, offsets, errno)
	}

	return &linuxGpioLines{fd: int(req.FD), n: len(offsets)}, nil
}

func (h *linuxGpioLines) SetValues(mask, bits uint64) error {
	v := gpioV2LineValues{Bits: bits, Mask: mask}
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(h.fd), gpioV2LineSetValuesIoctl, uintptr(unsafe.Pointer(&v))); errno != 0 {
		return errno
	}
	return nil
}

func (h *linuxGpioLines) GetValues() (uint64, error) {
	v := gpioV2LineValues{Mask: lineMask(h.n)}
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(h.fd), gpioV2LineGetValuesIoctl, uintptr(unsafe.Pointer(&v))); errno != 0 {
		return 0, errno
	}
	return v.Bits, nil
}

func (h *linuxGpioLines) Close() error {
	return syscall.Close(h.fd)
}

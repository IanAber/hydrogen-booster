package main

import "unsafe"

// This file reproduces the relevant parts of <linux/gpio.h> (the GPIO
// character device uAPI v2, kernel 5.10+): struct layouts and the ioctl
// request codes computed from them via the generic _IOC macro. It has no
// build tag - the definitions are pure arithmetic/struct layout with no
// syscalls, so gpioioctl_test.go can check them on any platform (see
// that file for why this is trustworthy despite being hand-rolled
// instead of vendored from a header). The actual syscalls live in
// gpiochip_linux.go, which is Linux-only.

const (
	gpioMaxNameSize       = 32
	gpioV2LinesMax        = 64
	gpioV2LineNumAttrsMax = 10
)

// gpio_v2_line_flag bits (only the ones this driver uses).
const gpioV2LineFlagOutput = 1 << 3

const gpioV2LineAttrIDOutputValues = 2

type gpiochipInfo struct {
	Name  [gpioMaxNameSize]byte
	Label [gpioMaxNameSize]byte
	Lines uint32
}

type gpioV2LineValues struct {
	Bits uint64
	Mask uint64
}

type gpioV2LineAttribute struct {
	ID      uint32
	Padding uint32
	Value   uint64 // union: flags / output values bitmask / debounce_period_us
}

type gpioV2LineConfigAttribute struct {
	Attr gpioV2LineAttribute
	Mask uint64
}

type gpioV2LineConfig struct {
	Flags    uint64
	NumAttrs uint32
	Padding  [5]uint32
	Attrs    [gpioV2LineNumAttrsMax]gpioV2LineConfigAttribute
}

type gpioV2LineRequest struct {
	Offsets         [gpioV2LinesMax]uint32
	Consumer        [gpioMaxNameSize]byte
	Config          gpioV2LineConfig
	NumLines        uint32
	EventBufferSize uint32
	Padding         [5]uint32
	FD              int32
}

// ioc reproduces the generic Linux _IOC(dir, type, nr, size) macro
// (include/uapi/asm-generic/ioctl.h) - stable across every architecture
// this project targets (amd64/arm64 dev machines, arm64 on the Pi).
func ioc(dir, typ, nr, size uintptr) uintptr {
	const (
		nrShift   = 0
		typeShift = nrShift + 8
		sizeShift = typeShift + 8
		dirShift  = sizeShift + 14
	)
	return (dir << dirShift) | (typ << typeShift) | (nr << nrShift) | (size << sizeShift)
}

func ior(typ, nr byte, size uintptr) uintptr  { return ioc(2, uintptr(typ), uintptr(nr), size) }
func iowr(typ, nr byte, size uintptr) uintptr { return ioc(3, uintptr(typ), uintptr(nr), size) }

const gpioIoctlType = 0xB4

var (
	gpioGetChipInfoIoctl     = ior(gpioIoctlType, 0x01, unsafe.Sizeof(gpiochipInfo{}))
	gpioV2GetLineIoctl       = iowr(gpioIoctlType, 0x07, unsafe.Sizeof(gpioV2LineRequest{}))
	gpioV2LineGetValuesIoctl = iowr(gpioIoctlType, 0x0E, unsafe.Sizeof(gpioV2LineValues{}))
	gpioV2LineSetValuesIoctl = iowr(gpioIoctlType, 0x0F, unsafe.Sizeof(gpioV2LineValues{}))
)

func lineMask(n int) uint64 {
	if n >= 64 {
		return ^uint64(0)
	}
	return (uint64(1) << uint(n)) - 1
}

func cString(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

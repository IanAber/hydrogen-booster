package main

import (
	"testing"
	"unsafe"
)

// TestGpioIoctlNumbersMatchKernelHeader cross-checks the hand-rolled
// struct layouts in gpioioctl.go against <linux/gpio.h> without needing
// a Linux build to do it: the ioctl request codes below are public,
// well-known constants (documented in the kernel source and reproduced
// in libgpiod and countless other GPIO tools), and each one is computed
// here from this file's own _IOC macro plus unsafe.Sizeof of this file's
// own struct definitions. If a struct's field order, padding, or a magic
// number were wrong, the computed value would almost certainly stop
// matching - because the ioctl code encodes the exact struct size, this
// is really a check on both the ioctl numbers AND the struct layout at
// once.
func TestGpioIoctlNumbersMatchKernelHeader(t *testing.T) {
	cases := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"GPIO_GET_CHIPINFO_IOCTL", gpioGetChipInfoIoctl, 0x8044b401},
		{"GPIO_V2_GET_LINE_IOCTL", gpioV2GetLineIoctl, 0xc250b407},
		{"GPIO_V2_LINE_GET_VALUES_IOCTL", gpioV2LineGetValuesIoctl, 0xc010b40e},
		{"GPIO_V2_LINE_SET_VALUES_IOCTL", gpioV2LineSetValuesIoctl, 0xc010b40f},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = 0x%x, want 0x%x (struct layout or ioctl number drifted from the kernel header)", c.name, c.got, c.want)
		}
	}
}

func TestGpioStructSizes(t *testing.T) {
	// struct gpiochip_info: char name[32] + char label[32] + __u32 lines
	if got, want := int(unsafe.Sizeof(gpiochipInfo{})), 68; got != want {
		t.Errorf("sizeof(gpiochipInfo) = %d, want %d", got, want)
	}
	// struct gpio_v2_line_values: __u64 bits + __u64 mask
	if got, want := int(unsafe.Sizeof(gpioV2LineValues{})), 16; got != want {
		t.Errorf("sizeof(gpioV2LineValues) = %d, want %d", got, want)
	}
	// struct gpio_v2_line_request: offsets[64]*4 + consumer[32] +
	// config(272) + num_lines(4) + event_buffer_size(4) + padding[5]*4 + fd(4)
	if got, want := int(unsafe.Sizeof(gpioV2LineRequest{})), 592; got != want {
		t.Errorf("sizeof(gpioV2LineRequest) = %d, want %d", got, want)
	}
}

func TestLineMask(t *testing.T) {
	if got, want := lineMask(1), uint64(0x1); got != want {
		t.Errorf("lineMask(1) = %#x, want %#x", got, want)
	}
	if got, want := lineMask(5), uint64(0x1f); got != want {
		t.Errorf("lineMask(5) = %#x, want %#x", got, want)
	}
	if got, want := lineMask(64), ^uint64(0); got != want {
		t.Errorf("lineMask(64) = %#x, want %#x", got, want)
	}
}

func TestCString(t *testing.T) {
	buf := make([]byte, 32)
	copy(buf, "gpiochip16")
	if got, want := cString(buf), "gpiochip16"; got != want {
		t.Errorf("cString = %q, want %q", got, want)
	}
}

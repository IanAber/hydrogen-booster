package main

import (
	"encoding/binary"
	"fmt"
	"net"
	"syscall"
	"time"
	"unsafe"
)

// SocketCanTransport talks to a real Linux SocketCAN interface (e.g.
// can0 on the CM5 board) using a raw AF_CAN/CAN_RAW socket - the same
// mechanism the `cansend`/`candump` command-line tools use.
//
// It expects the interface to already be up and configured with the
// correct bitrate (e.g. `ip link set can0 up type can bitrate 500000`) -
// normally brought up at boot by a systemd/netplan unit on the CM5
// image, not by this program. Only stdlib packages are used (syscall,
// net); no external CAN library or golang.org/x/sys dependency.
type SocketCanTransport struct {
	fd int
}

const (
	afCAN  = 29 // syscall.AF_CAN
	canRaw = 1  // CAN_RAW protocol, from linux/can.h
)

// rawSockaddrCAN mirrors the kernel's struct sockaddr_can. For CAN_RAW,
// only Family and Ifindex are meaningful; the rest is the unused
// transport-address union, kept only so the struct is the correct size
// for bind().
type rawSockaddrCAN struct {
	Family  uint16
	Ifindex int32
	_       [8]byte
}

// OpenSocketCAN opens and binds a raw CAN_RAW socket to the named
// interface. It's the real hardware driver behind the IO/CAN
// abstraction - PressureModule (pressuremodule.go) calls this (via the
// openFn it's given in main.go) only once live data is switched on from
// the admin screen, so nothing touches the bus, and no interface needs
// to exist, until an operator asks for it.
func OpenSocketCAN(ifaceName string) (CanTransport, error) {
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return nil, fmt.Errorf("interface %s: %w", ifaceName, err)
	}

	fd, err := syscall.Socket(afCAN, syscall.SOCK_RAW, canRaw)
	if err != nil {
		return nil, fmt.Errorf("socket: %w", err)
	}

	addr := rawSockaddrCAN{Family: uint16(afCAN), Ifindex: int32(iface.Index)}
	_, _, errno := syscall.Syscall(syscall.SYS_BIND, uintptr(fd),
		uintptr(unsafe.Pointer(&addr)), unsafe.Sizeof(addr))
	if errno != 0 {
		syscall.Close(fd)
		return nil, fmt.Errorf("bind %s: %w", ifaceName, errno)
	}

	return &SocketCanTransport{fd: fd}, nil
}

// Send writes one classic CAN frame: struct can_frame is 16 bytes -
// 4-byte ID, 1-byte DLC, 3 reserved/pad bytes, then 8 data bytes.
func (t *SocketCanTransport) Send(f CanFrame) error {
	buf := make([]byte, 16)
	binary.LittleEndian.PutUint32(buf[0:4], f.ID)
	buf[4] = f.Len
	// buf[5:8] reserved/pad, left zero
	copy(buf[8:16], f.Data[:])

	_, err := syscall.Write(t.fd, buf)
	return err
}

// Recv reads one classic CAN frame, waiting up to timeout.
func (t *SocketCanTransport) Recv(timeout time.Duration) (CanFrame, error) {
	tv := syscall.NsecToTimeval(timeout.Nanoseconds())
	if err := syscall.SetsockoptTimeval(t.fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv); err != nil {
		return CanFrame{}, fmt.Errorf("set receive timeout: %w", err)
	}

	buf := make([]byte, 16)
	n, err := syscall.Read(t.fd, buf)
	if err != nil {
		return CanFrame{}, err
	}
	if n < 16 {
		return CanFrame{}, fmt.Errorf("short CAN frame read (%d bytes)", n)
	}

	var f CanFrame
	// Mask off the EFF/RTR/ERR flag bits in the top 3 bits of can_id;
	// this module only uses standard 11-bit IDs.
	f.ID = binary.LittleEndian.Uint32(buf[0:4]) & 0x1FFFFFFF
	f.Len = buf[4]
	copy(f.Data[:], buf[8:16])
	return f, nil
}

func (t *SocketCanTransport) Close() error {
	return syscall.Close(t.fd)
}

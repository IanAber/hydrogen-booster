//go:build linux

package main

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// This file talks to a Linux tty device directly via termios ioctls -
// stdlib + golang.org/x/sys/unix only (already a transitive dependency
// via modernc.org/sqlite, so no new module), matching gpiochip_linux.go's
// "hand-roll the Linux-specific ioctl, no external library" approach.

// baudRateConstants maps a plain integer baud rate to the termios
// B-constant Cflag expects - only the common rates are listed; the
// Aalborg XFM manual specifies 9600.
var baudRateConstants = map[int]uint32{
	1200: unix.B1200, 2400: unix.B2400, 4800: unix.B4800,
	9600: unix.B9600, 19200: unix.B19200, 38400: unix.B38400,
	57600: unix.B57600, 115200: unix.B115200,
}

// linuxSerialPort is the real SerialPort implementation.
type linuxSerialPort struct {
	f *os.File
}

// OpenSerialPort opens path (e.g. "/dev/ttyUSB0") in raw mode at baud,
// 8 data bits, no parity, 1 stop bit, no flow control - the fixed
// settings the Aalborg XFM series' ASCII command interface requires
// (see the XFM operating manual's "Communication Parameters" section).
func OpenSerialPort(path string, baud int) (SerialPort, error) {
	speed, ok := baudRateConstants[baud]
	if !ok {
		return nil, fmt.Errorf("unsupported baud rate %d", baud)
	}

	f, err := os.OpenFile(path, os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	fd := int(f.Fd())
	t, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("get termios for %s: %w", path, err)
	}

	// Raw mode: no line editing/echo/signals, no input/output character
	// translation - we want exactly the bytes the device sends, and to
	// send exactly the bytes we write.
	t.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	t.Oflag &^= unix.OPOST
	t.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	t.Cflag &^= unix.CSIZE | unix.PARENB | unix.CSTOPB | unix.CRTSCTS | unix.CBAUD
	t.Cflag |= unix.CS8 | unix.CLOCAL | unix.CREAD | speed

	// VMIN=0, VTIME=10 (1.0s): a read() call returns as soon as any
	// byte arrives, or after 1s with nothing at all - a bounded
	// per-call timeout rather than blocking forever on a disconnected
	// or unresponsive device. See linuxSerialPort.Read below for why a
	// timed-out (0, nil) read is translated into an error.
	t.Cc[unix.VMIN] = 0
	t.Cc[unix.VTIME] = 10

	if err := unix.IoctlSetTermios(fd, unix.TCSETS, t); err != nil {
		f.Close()
		return nil, fmt.Errorf("set termios for %s: %w", path, err)
	}

	return &linuxSerialPort{f: f}, nil
}

// Read returns an error rather than (0, nil) on a VTIME timeout -
// bufio.Reader (used to assemble CR-terminated response lines in
// flowmodule.go) only tolerates a handful of consecutive zero-byte,
// no-error reads before giving up, which at a 1s VTIME would take far
// longer than any reasonable command timeout should.
func (p *linuxSerialPort) Read(b []byte) (int, error) {
	n, err := p.f.Read(b)
	if err == nil && n == 0 {
		return 0, fmt.Errorf("serial read timeout on %s", p.f.Name())
	}
	return n, err
}

func (p *linuxSerialPort) Write(b []byte) (int, error) { return p.f.Write(b) }
func (p *linuxSerialPort) Close() error                { return p.f.Close() }

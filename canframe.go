package main

import (
	"encoding/binary"
	"fmt"
	"time"
)

// CanFrame is a transport-agnostic classic (non-FD) CAN frame: up to 8
// data bytes, matching what `cansend`/`candump` and Linux SocketCAN work
// with.
type CanFrame struct {
	ID   uint32
	Len  uint8
	Data [8]byte
}

// CanTransport is the seam between the pressure-module protocol logic
// below (which is plain, hardware-free Go, and unit-tested without any
// CAN bus) and the actual bus. SocketCanTransport (cansocket_linux.go)
// implements this against a real Linux SocketCAN interface; tests use a
// fake implementation instead (see pressuremodule_test.go).
type CanTransport interface {
	Send(f CanFrame) error
	Recv(timeout time.Duration) (CanFrame, error)
	Close() error
}

// BuildReadAllRequest builds the "read all channels" request frame. For
// the default config this is exactly `101#020006` - function code 0x02,
// start offset 0x00, channel count 0x06 - matching `cansend can0
// 101#020006` on the real module.
func BuildReadAllRequest(cfg CanModuleConfig) CanFrame {
	f := CanFrame{ID: cfg.RequestID, Len: 3}
	f.Data[0] = cfg.FunctionCode
	f.Data[1] = 0x00
	f.Data[2] = byte(cfg.ChannelCount)
	return f
}

// ParsedResponse is one decoded response frame. The module replies with
// ceil(ChannelCount/3) frames, each carrying up to three big-endian
// 16-bit raw channel values starting at a given channel offset - e.g.
// for 6 channels: one frame with offset 0 covering channels 0,1,2, and
// one with offset 3 covering channels 3,4,5, matching the confirmed
// module output:
//
//	02 00 00 00 00 00 00 00   (func 0x02, offset 0, channels 0-2)
//	02 03 00 00 00 00 00 00   (func 0x02, offset 3, channels 3-5)
type ParsedResponse struct {
	FuncCode byte
	Offset   int
	Values   []int // up to 3 raw 16-bit values, big-endian, high byte first
}

func ParseResponseFrame(f CanFrame) (ParsedResponse, error) {
	if f.Len < 2 {
		return ParsedResponse{}, fmt.Errorf("response frame too short (%d bytes)", f.Len)
	}
	r := ParsedResponse{FuncCode: f.Data[0], Offset: int(f.Data[1])}
	n := int(f.Len) - 2
	for i := 0; i+1 < n; i += 2 {
		r.Values = append(r.Values, int(binary.BigEndian.Uint16(f.Data[2+i:4+i])))
	}
	return r, nil
}

package main

import (
	"errors"
	"time"
)

// fakeCanTransport is an in-memory CanTransport for tests: Send records
// what was sent; Recv pops frames off a preloaded queue.
type fakeCanTransport struct {
	sent   []CanFrame
	toRecv []CanFrame
	closed bool
}

func (f *fakeCanTransport) Send(fr CanFrame) error {
	f.sent = append(f.sent, fr)
	return nil
}

func (f *fakeCanTransport) Recv(timeout time.Duration) (CanFrame, error) {
	if len(f.toRecv) == 0 {
		return CanFrame{}, errors.New("fakeCanTransport: no more frames queued")
	}
	fr := f.toRecv[0]
	f.toRecv = f.toRecv[1:]
	return fr, nil
}

func (f *fakeCanTransport) Close() error {
	f.closed = true
	return nil
}

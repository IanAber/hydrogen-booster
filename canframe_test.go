package main

import "testing"

func TestBuildReadAllRequestMatchesConfirmedExample(t *testing.T) {
	cfg := DefaultConfig().CanModule
	f := BuildReadAllRequest(cfg)

	// Must match `cansend can0 101#020006` exactly.
	if f.ID != 0x101 {
		t.Errorf("ID = 0x%X, want 0x101", f.ID)
	}
	if f.Len != 3 {
		t.Errorf("Len = %d, want 3", f.Len)
	}
	if f.Data[0] != 0x02 || f.Data[1] != 0x00 || f.Data[2] != 0x06 {
		t.Errorf("Data = % X, want 02 00 06", f.Data[:3])
	}
}

func TestParseResponseFrameMatchesConfirmedExample(t *testing.T) {
	// The two frames from the real module, exactly as reported:
	//   02 00 00 00 00 00 00 00
	//   02 03 00 00 00 00 00 00
	f1 := CanFrame{ID: 0x001, Len: 8, Data: [8]byte{0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}}
	f2 := CanFrame{ID: 0x001, Len: 8, Data: [8]byte{0x02, 0x03, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}}

	r1, err := ParseResponseFrame(f1)
	if err != nil {
		t.Fatal(err)
	}
	if r1.FuncCode != 0x02 || r1.Offset != 0 || len(r1.Values) != 3 {
		t.Fatalf("frame1 parsed as %+v", r1)
	}
	for i, v := range r1.Values {
		if v != 0 {
			t.Errorf("frame1 value[%d] = %d, want 0", i, v)
		}
	}

	r2, err := ParseResponseFrame(f2)
	if err != nil {
		t.Fatal(err)
	}
	if r2.FuncCode != 0x02 || r2.Offset != 3 || len(r2.Values) != 3 {
		t.Fatalf("frame2 parsed as %+v", r2)
	}
}

func TestParseResponseFrameBigEndianValues(t *testing.T) {
	// channel value bytes 0x1234, 0x0001, 0xFFFF - confirms high-byte-first decoding.
	f := CanFrame{ID: 0x001, Len: 8, Data: [8]byte{0x02, 0x00, 0x12, 0x34, 0x00, 0x01, 0xFF, 0xFF}}
	r, err := ParseResponseFrame(f)
	if err != nil {
		t.Fatal(err)
	}
	want := []int{0x1234, 0x0001, 0xFFFF}
	if len(r.Values) != 3 {
		t.Fatalf("got %d values, want 3", len(r.Values))
	}
	for i, v := range want {
		if r.Values[i] != v {
			t.Errorf("Values[%d] = 0x%X, want 0x%X", i, r.Values[i], v)
		}
	}
}

func TestParseResponseFrameTooShort(t *testing.T) {
	f := CanFrame{ID: 0x001, Len: 1, Data: [8]byte{0x02}}
	if _, err := ParseResponseFrame(f); err == nil {
		t.Fatal("expected an error for a frame shorter than 2 bytes")
	}
}

func TestChannelCalibrationLinear(t *testing.T) {
	ch := CanChannelConfig{CalLowRaw: 0, CalLowBar: 0, CalHighRaw: 65535, CalHighBar: 250}

	cases := []struct {
		raw     int
		wantBar float64
		tol     float64
	}{
		{0, 0, 0.01},
		{65535, 250, 0.01},
		{32768, 125, 0.05}, // ~midpoint
	}
	for _, c := range cases {
		got := ch.ToBar(c.raw)
		if got < c.wantBar-c.tol || got > c.wantBar+c.tol {
			t.Errorf("ToBar(%d) = %v, want ~%v", c.raw, got, c.wantBar)
		}
	}
}

func TestChannelCalibrationOffsetRange(t *testing.T) {
	// A calibration where 0 bar isn't at raw 0 - e.g. observed 1000 counts
	// at atmospheric (0 bar gauge), 60000 counts at 300 bar.
	ch := CanChannelConfig{CalLowRaw: 1000, CalLowBar: 0, CalHighRaw: 60000, CalHighBar: 300}

	if got := ch.ToBar(1000); got != 0 {
		t.Errorf("ToBar(1000) = %v, want 0", got)
	}
	if got := ch.ToBar(60000); got != 300 {
		t.Errorf("ToBar(60000) = %v, want 300", got)
	}
	// Below the low point should extrapolate below zero, not clamp -
	// useful for spotting a miscalibrated/disconnected sensor rather
	// than silently hiding it at zero.
	if got := ch.ToBar(0); got >= 0 {
		t.Errorf("ToBar(0) = %v, want negative (extrapolated below the low calibration point)", got)
	}
}

func TestChannelCalibrationDegenerateRange(t *testing.T) {
	// CalHighRaw == CalLowRaw should never divide by zero.
	ch := CanChannelConfig{CalLowRaw: 100, CalLowBar: 5, CalHighRaw: 100, CalHighBar: 50}
	if got := ch.ToBar(100); got != 5 {
		t.Errorf("ToBar with degenerate range = %v, want the low value (5)", got)
	}
}

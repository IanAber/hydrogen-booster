package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestWriteErrorSendsMessageHeaderAndPlainBody(t *testing.T) {
	rec := httptest.NewRecorder()
	writeError(rec, httptest.NewRequest(http.MethodPost, "/api/start", nil), msgErr("target_pressure_range", map[string]any{"max": "350"}, "target pressure must be between 0 and %d bar for the selected nozzle", 350), http.StatusBadRequest)

	if got := rec.Body.String(); got != "target pressure must be between 0 and 350 bar for the selected nozzle\n" {
		t.Errorf("body = %q, want the English text unchanged", got)
	}
	raw, err := url.PathUnescape(rec.Header().Get("X-Message"))
	if err != nil {
		t.Fatal(err)
	}
	var m Message
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	if m.Code != "target_pressure_range" || m.Params["max"] != "350" {
		t.Errorf("header message = %+v", m)
	}
}

func TestMessageOfPlainErrorIsUntranslatedText(t *testing.T) {
	m := messageOf(errors.New("recv: timeout"))
	if m.Code != "" || m.Text != "recv: timeout" {
		t.Errorf("messageOf = %+v", m)
	}
}

func TestSensorFaultCarriesNestedCodes(t *testing.T) {
	c, io := newSensorFaultController(t, DefaultConfig())
	io.badSensor[OutputTemp] = withLastErr(msg("no_reading_for", map[string]any{"seconds": "3.1"}, "no reading for 3.1s"), "CRC check failed")
	c.superviseSafety()

	details := c.Status().FaultDetails
	if len(details) != 1 {
		t.Fatalf("faultDetails = %+v", details)
	}
	f := details[0]
	if f.Code != "emergency_stop" {
		t.Fatalf("code = %q, want emergency_stop", f.Code)
	}
	inner, ok := f.Params["reason"].(Message)
	if !ok || inner.Code != "sensor_unreadable" || inner.Params["sensor"] != "OutputTemp" {
		t.Fatalf("reason = %+v", f.Params["reason"])
	}
	why, ok := inner.Params["reason"].(Message)
	if !ok || why.Code != "no_reading_for" {
		t.Fatalf("unreadable reason = %+v", inner.Params["reason"])
	}
	want := "Hydrogen output temperature sensor unreadable (no reading for 3.1s; last error: CRC check failed) - emergency stop"
	if f.Text != want {
		t.Errorf("text = %q, want %q", f.Text, want)
	}
}

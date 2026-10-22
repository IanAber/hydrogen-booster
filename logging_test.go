package main

import (
	"bytes"
	"errors"
	"log"
	"strings"
	"testing"
)

func captureLog(t *testing.T, journal bool) *bytes.Buffer {
	var buf bytes.Buffer
	prevOut, prevFlags, prevJournal := log.Writer(), log.Flags(), logToJournal
	log.SetOutput(&buf)
	log.SetFlags(0)
	logToJournal = journal
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
		logToJournal = prevJournal
	})
	return &buf
}

func TestLogPriorityPrefixOnlyUnderJournal(t *testing.T) {
	buf := captureLog(t, true)
	logErrorf("boom %d", 1)
	logWarnf("hmm")
	if got := buf.String(); got != "<3>boom 1\n<4>hmm\n" {
		t.Errorf("journal output = %q", got)
	}

	buf = captureLog(t, false)
	logErrorf("boom")
	if got := buf.String(); got != "boom\n" {
		t.Errorf("plain output = %q, want no priority prefix", got)
	}
}

func TestErrorLatchLogsChangesAndRecoveryOnly(t *testing.T) {
	buf := captureLog(t, false)
	var l errorLatch
	for i := 0; i < 5; i++ {
		l.report("CAN", errors.New("recv: timeout"))
	}
	l.report("CAN", errors.New("send: no buffer space"))
	l.report("CAN", nil)
	l.report("CAN", nil)

	want := "CAN: recv: timeout\nCAN: send: no buffer space\nCAN: recovered\n"
	if got := buf.String(); got != want {
		t.Errorf("log = %q, want %q", got, want)
	}
}

func TestEveryNewFaultIsLogged(t *testing.T) {
	buf := captureLog(t, false)
	c, io := newSensorFaultController(t, DefaultConfig())
	io.badSensor[TankPressure] = errors.New("recv: timeout")
	c.superviseSafety()
	c.superviseSafety()
	io.badSensor[OutputTemp] = errors.New("CRC check failed")
	c.superviseSafety()
	delete(io.badSensor, TankPressure)
	c.superviseSafety()
	c.ClearFault()

	out := buf.String()
	for _, want := range []string{
		"FAULT: Storage tank pressure sensor unreadable",
		"FAULT: Hydrogen output temperature sensor unreadable",
		"Sensor problem resolved: Storage tank pressure sensor unreadable",
		"Fault cleared by operator (2 fault(s) acknowledged)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q:\n%s", want, out)
		}
	}
	if n := strings.Count(out, "FAULT: Storage tank pressure"); n != 1 {
		t.Errorf("tank fault logged %d times, want once:\n%s", n, out)
	}
}

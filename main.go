package main

import (
	"flag"
	"log"
	"time"
)

func main() {
	configPath := flag.String("config", "config.json",
		"path to the human-readable JSON config file (timing/pressure settings only; "+
			"relay/sensor wiring is still set in config.go until the web-based IO "+
			"configuration screen is added)")
	listenAddr := flag.String("listen", "",
		"override the web UI/API listen address (e.g. :8081); if empty, uses the config default")
	webDir := flag.String("web", "web",
		"path to the admin web UI's static assets directory")
	adminPasswordFile := flag.String("admin-password-file", "admin-password.txt",
		"path to a file holding the admin password for the operator/admin/diagnostics "+
			"screens (plain text, one line); auto-generated on first run if missing")
	dispenseLogFile := flag.String("dispense-log-file", "dispense-log.jsonl",
		"path to the file completed fills are appended to (one JSON object per line); "+
			"GET /api/records returns and clears it")
	validCodesFile := flag.String("valid-codes-file", "valid-codes.txt",
		"path to the admin-maintained customer code allow-list (plain text, "+
			"\"code,Full Name\" per line); a fill's customer code must appear here")
	historyDBFile := flag.String("history-db", "history.db",
		"path to the SQLite database storing pressure/temperature/valve history; "+
			"queried via GET /api/history/*")
	historySampleInterval := flag.Duration("history-sample-interval", 5*time.Second,
		"how often to sample pressure/temperature readings into the history database")
	historyRetentionDays := flag.Int("history-retention-days", 30,
		"how many days of history to keep before older rows are pruned")
	flag.Parse()

	adminAuth, err := LoadOrCreateAdminAuth(*adminPasswordFile)
	if err != nil {
		log.Fatalf("admin password: %v", err)
	}

	dispenseLog := NewDispenseLog(*dispenseLogFile)

	validCodes, err := LoadValidCodes(*validCodesFile)
	if err != nil {
		log.Fatalf("valid codes: %v", err)
	}

	base := DefaultConfig()
	cfg, loaded, err := LoadConfigFile(*configPath, base)
	switch {
	case err != nil:
		log.Printf("Could not load config file %s (%v) - using built-in defaults", *configPath, err)
		cfg = base
	case loaded:
		log.Printf("Loaded configuration from %s", *configPath)
	default:
		log.Printf("No config file at %s yet - using built-in defaults (use 'Apply & Save to File' on the admin screen to create it)", *configPath)
	}
	if *listenAddr != "" {
		cfg.ListenAddr = *listenAddr
	}
	live := NewLiveConfig(cfg)

	// Real hardware driver #1: the 6-channel CAN pressure input module.
	// It only opens can0 once live data is switched on from the admin
	// screen; until then (and for any sensor not assigned to it) values
	// come from SimulatedIO below, per the "simulate now, real drivers
	// supplied later" plan - this is the first of those drivers.
	pm := NewPressureModule(live, OpenSocketCAN)
	go pm.Run()

	// Real hardware driver #2: DS18B20 1-Wire temperature sensors via
	// /sys/bus/w1/devices. Same live/simulation switch as the pressure
	// module; only reads sensors an operator has assigned from the
	// admin screen.
	tm := NewTemperatureModule(live, readOneWireTempC)
	go tm.Run()

	// Real hardware driver #3: MCP23017 relay outputs via the
	// /dev/gpiochip16 character device. Same live/simulation switch;
	// only drives valves an operator has assigned a port to from the
	// admin screen.
	rm := NewRelayModule(live, OpenGpioLines)
	go rm.Run()
	defer rm.Stop()

	// Real hardware driver #4: digital feedback inputs (hydraulic oil
	// level, motor-powered confirmation) via the SoC's own GPIO
	// controller, a different chip than the relay expander above. Same
	// live/simulation switch; only reads signals an operator has
	// assigned a line to from the admin screen.
	dim := NewDigitalInputModule(live, OpenDigitalInputLines)
	go dim.Run()
	defer dim.Stop()

	// Real hardware driver #5: an optional real mass flow sensor
	// (currently only the Aalborg XFM series, over a serial link).
	// Unlike the modules above, this one isn't gated by its own
	// LiveEnabled flag - it only connects while BOTH
	// Config.MassMeasurementEnabled and FlowModuleConfig.SensorType
	// select it (see FlowModule's doc comment in flowmodule.go).
	// Controller.deliveredMassKg falls back to the tank
	// pressure-differential estimate whenever this isn't the case.
	fm := NewFlowModule(live, OpenSerialPort)
	go fm.Run()
	defer fm.Stop()

	// Everything else - any relay/pressure/temperature/digital-input
	// channel not assigned to a real module above - is still simulated
	// pending real drivers for that hardware.
	hw := NewSimulatedIO()
	defer hw.Close()

	io := NewCompositeIO(hw, pm, tm, rm, dim, fm)

	ctrl := NewController(live, io)
	ctrl.SetDispenseLog(dispenseLog)
	ctrl.SetValidCodes(validCodes)
	go ctrl.Run()

	// Background history log: pressure/temperature sampled on a fixed
	// interval, valve/relay/digital-input states logged only on change.
	// Runs continuously regardless of job phase - see historylogger.go.
	history, err := NewHistoryStore(*historyDBFile)
	if err != nil {
		log.Fatalf("history database: %v", err)
	}
	defer history.Close()
	histLogger := NewHistoryLogger(history, io, *historySampleInterval, time.Duration(*historyRetentionDays)*24*time.Hour)
	go histLogger.Run()

	srv := NewWebServer(ctrl, live, pm, tm, dim, fm, history, cfg.ListenAddr, *configPath, *webDir, adminAuth, dispenseLog, validCodes)
	log.Fatal(srv.ListenAndServe())
}

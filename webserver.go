package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type WebServer struct {
	ctrl        *Controller
	live        *LiveConfig
	pm          *PressureModule
	tm          *TemperatureModule
	dim         *DigitalInputModule
	fm          *FlowModule
	history     *HistoryStore
	listenAddr  string
	configPath  string
	webDir      string
	auth        *AdminAuth
	dispenseLog *DispenseLog
	validCodes  *ValidCodes
}

func NewWebServer(ctrl *Controller, live *LiveConfig, pm *PressureModule, tm *TemperatureModule, dim *DigitalInputModule, fm *FlowModule, history *HistoryStore, listenAddr, configPath, webDir string, auth *AdminAuth, dispenseLog *DispenseLog, validCodes *ValidCodes) *WebServer {
	return &WebServer{
		ctrl: ctrl, live: live, pm: pm, tm: tm, dim: dim, fm: fm, history: history,
		listenAddr: listenAddr, configPath: configPath, webDir: webDir,
		auth: auth, dispenseLog: dispenseLog, validCodes: validCodes,
	}
}

// serveStatic serves exactly one file from webDir, regardless of the
// request path - used for the staff-only pages (operator.html,
// admin.html, diagnostics.html) so they can be registered as exact
// routes and wrapped in requireAuth, ahead of the public "/" catch-all.
func (w *WebServer) serveStatic(name string) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		http.ServeFile(rw, r, filepath.Join(w.webDir, name))
	}
}

// ListenAndServe wires up routes in three access tiers:
//
//   - Public-from-anywhere: the customer screen (index.html) and the
//     job-control API it needs (status/start/stop), plus shared static
//     assets (i18n.js, images/*) every page - including the staff ones -
//     loads.
//   - Public-from-localhost-only, auth required otherwise: the same
//     customer screen's API when reached over the network rather than
//     from the on-site kiosk. This is requireAuthUnlessLocal on "/" and
//     the job-control endpoints - see auth.go.
//   - Read-public-from-localhost-only, write always staff-only:
//     GET /api/config - the customer screen's initI18n() reads system
//     name/language/mass-measurement/flow-sensor settings from it on
//     every page load, so it needs the same local exception as the
//     tier above, but POSTing a new config must always require the
//     admin login even from the kiosk itself. See
//     requireAuthUnlessLocalRead in auth.go.
//   - Staff-only, always auth required regardless of origin: the
//     operator/admin/diagnostics pages and every other API endpoint
//     that lets you reconfigure the system, read raw diagnostics, or
//     adjust simulated sensor values.
func (w *WebServer) ListenAndServe() error {
	if _, err := os.Stat(w.webDir); err != nil {
		return fmt.Errorf("web assets directory %s: %w", w.webDir, err)
	}
	mux := http.NewServeMux()

	// Public customer screen and shared assets: open from localhost (the
	// on-site kiosk), auth-gated from anywhere else on the network.
	mux.HandleFunc("/", w.requireAuthUnlessLocal(http.FileServer(http.Dir(w.webDir)).ServeHTTP))
	mux.HandleFunc("/api/status", w.requireAuthUnlessLocal(w.handleStatus))
	mux.HandleFunc("/api/start", w.requireAuthUnlessLocal(w.handleStart))
	mux.HandleFunc("/api/stop", w.requireAuthUnlessLocal(w.handleStop))
	// Looks up one code at a time and only ever returns that one code's
	// name (never the list) - safe to leave at the same access tier as
	// start/stop, which is what actually enforces the allow-list anyway.
	mux.HandleFunc("/api/lookup-code", w.requireAuthUnlessLocal(w.handleLookupCode))

	// Staff-only pages and APIs: always require the admin login.
	mux.HandleFunc("/operator.html", w.requireAuth(w.serveStatic("operator.html")))
	mux.HandleFunc("/admin.html", w.requireAuth(w.serveStatic("admin.html")))
	mux.HandleFunc("/diagnostics.html", w.requireAuth(w.serveStatic("diagnostics.html")))
	mux.HandleFunc("/history.html", w.requireAuth(w.serveStatic("history.html")))
	mux.HandleFunc("/manual.html", w.requireAuth(w.serveStatic("manual.html")))
	mux.HandleFunc("/api/diagnostics", w.requireAuth(w.handleDiagnostics))
	mux.HandleFunc("/api/clearfault", w.requireAuth(w.handleClearFault))
	mux.HandleFunc("/api/manual/enable", w.requireAuth(w.handleManualEnable))
	mux.HandleFunc("/api/manual/pump", w.requireAuth(w.handleManualPump))
	mux.HandleFunc("/api/manual/jog", w.requireAuth(w.handleManualJog))
	mux.HandleFunc("/api/config", w.requireAuthUnlessLocalRead(w.handleConfig))
	mux.HandleFunc("/api/can/status", w.requireAuth(w.handleCanStatus))
	mux.HandleFunc("/api/can/sim", w.requireAuth(w.handleCanSim))
	mux.HandleFunc("/api/temp/status", w.requireAuth(w.handleTempStatus))
	mux.HandleFunc("/api/temp/sim", w.requireAuth(w.handleTempSim))
	mux.HandleFunc("/api/digital/status", w.requireAuth(w.handleDigitalStatus))
	mux.HandleFunc("/api/digital/sim", w.requireAuth(w.handleDigitalSim))
	mux.HandleFunc("/api/flow/status", w.requireAuth(w.handleFlowStatus))
	mux.HandleFunc("/api/flow/reset", w.requireAuth(w.handleFlowReset))
	mux.HandleFunc("/api/admin/password", w.requireAuth(w.handleChangePassword))
	mux.HandleFunc("/api/records", w.requireAuth(w.handleRecords))
	mux.HandleFunc("/api/admin/codes", w.requireAuth(w.handleAdminCodes))
	mux.HandleFunc("/docs.html", w.requireAuth(w.serveStatic("docs.html")))
	mux.HandleFunc("/api/docs", w.requireAuth(w.handleAPIDocs))
	mux.HandleFunc("/api/history/channels", w.requireAuth(w.handleHistoryChannels))
	mux.HandleFunc("/api/history/readings", w.requireAuth(w.handleHistoryReadings))
	mux.HandleFunc("/api/history/valves", w.requireAuth(w.handleHistoryValves))

	log.Printf("Web UI listening on %s", w.listenAddr)
	return http.ListenAndServe(w.listenAddr, mux)
}

func (w *WebServer) handleStatus(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(w.ctrl.Status())
}

func (w *WebServer) handleDiagnostics(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(w.ctrl.Diagnostics())
}

type startRequest struct {
	TargetPressureBar float64 `json:"targetPressureBar"`
	TargetMassKg      float64 `json:"targetMassKg"`
	Nozzle            int     `json:"nozzle"`
	CustomerCode      string  `json:"customerCode"`
}

func (w *WebServer) handleStart(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req startRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(rw, "invalid request body", http.StatusBadRequest)
		return
	}
	job := JobRequest{
		TargetPressureBar: req.TargetPressureBar,
		TargetMassKg:      req.TargetMassKg,
		Nozzle:            NozzleSize(req.Nozzle),
		CustomerCode:      req.CustomerCode,
	}
	if err := w.ctrl.Start(job); err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	rw.WriteHeader(http.StatusOK)
}

func (w *WebServer) handleStop(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "POST required", http.StatusMethodNotAllowed)
		return
	}
	w.ctrl.Stop()
	rw.WriteHeader(http.StatusOK)
}

func (w *WebServer) handleClearFault(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "POST required", http.StatusMethodNotAllowed)
		return
	}
	w.ctrl.ClearFault()
	rw.WriteHeader(http.StatusOK)
}

type manualEnableRequest struct {
	Enable bool `json:"enable"`
}

// handleManualEnable turns manual piston control on or off - see
// Controller.SetManualMode. Current pressures and valve/pump states for
// the manual page itself are read from the existing /api/diagnostics
// endpoint rather than duplicated here.
func (w *WebServer) handleManualEnable(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req manualEnableRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(rw, "invalid request body", http.StatusBadRequest)
		return
	}
	if err := w.ctrl.SetManualMode(req.Enable); err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	rw.WriteHeader(http.StatusOK)
}

type manualPumpRequest struct {
	Stage int  `json:"stage"`
	On    bool `json:"on"`
}

// handleManualPump turns one stage's hydraulic pump motor on or off
// from the manual control page - see Controller.ManualSetPump.
func (w *WebServer) handleManualPump(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req manualPumpRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(rw, "invalid request body", http.StatusBadRequest)
		return
	}
	if err := w.ctrl.ManualSetPump(req.Stage, req.On); err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	rw.WriteHeader(http.StatusOK)
}

type manualJogRequest struct {
	Stage     int    `json:"stage"`
	Direction string `json:"direction"` // "up" or "down"
	Active    bool   `json:"active"`    // true while the button is held, false on release
}

// handleManualJog drives one stage's direction/relief valves while its
// Up/Down button is held on the manual control page - see
// Controller.ManualJog.
func (w *WebServer) handleManualJog(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req manualJogRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(rw, "invalid request body", http.StatusBadRequest)
		return
	}
	var up bool
	switch req.Direction {
	case "up":
		up = true
	case "down":
		up = false
	default:
		http.Error(rw, fmt.Sprintf("direction must be \"up\" or \"down\", got %q", req.Direction), http.StatusBadRequest)
		return
	}
	if err := w.ctrl.ManualJog(req.Stage, up, req.Active); err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	rw.WriteHeader(http.StatusOK)
}

type configResponse struct {
	Config         ConfigDTO `json:"config"`
	ConfigFilePath string    `json:"configFilePath"`
	Saved          bool      `json:"saved"`
	SaveError      string    `json:"saveError,omitempty"`
}

// handleConfig serves the admin screen.
//
// GET returns the current editable configuration plus the path of the
// config file in use.
//
// POST validates and applies a new configuration, taking effect on the
// very next control-loop tick - no restart required. Applying is
// in-memory only *unless* the request includes ?persist=true (or
// persist=1), in which case the same values are also written to the
// human-readable JSON config file at configPath, so they're picked up
// again on the next program start. "Save to file" is a separate action
// from "Apply" on the admin screen for exactly this reason: an operator
// can try a setting live before committing it to disk.
func (w *WebServer) handleConfig(rw http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(configResponse{
			Config:         ToDTO(w.live.Snapshot()),
			ConfigFilePath: w.configPath,
		})

	case http.MethodPost:
		var dto ConfigDTO
		if err := json.NewDecoder(r.Body).Decode(&dto); err != nil {
			http.Error(rw, "invalid request body", http.StatusBadRequest)
			return
		}
		merged, err := FromDTO(w.live.Snapshot(), dto)
		if err != nil {
			http.Error(rw, err.Error(), http.StatusBadRequest)
			return
		}
		w.live.Set(merged)

		resp := configResponse{Config: ToDTO(merged), ConfigFilePath: w.configPath}

		persist := r.URL.Query().Get("persist") == "true" || r.URL.Query().Get("persist") == "1"
		if persist {
			if err := SaveConfigFile(w.configPath, merged); err != nil {
				resp.SaveError = err.Error()
				log.Printf("Configuration applied, but failed to save to %s: %v", w.configPath, err)
			} else {
				resp.Saved = true
				log.Printf("Configuration applied and saved to %s", w.configPath)
			}
		} else {
			log.Printf("Configuration applied (in memory only, not saved to file)")
		}

		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(resp)

	default:
		http.Error(rw, "GET or POST required", http.StatusMethodNotAllowed)
	}
}

func (w *WebServer) handleCanStatus(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(w.pm.Status())
}

type canSimRequest struct {
	// Values maps sensor name (SensorInput.String(), e.g. "TankPressure")
	// to the bar value to set. Only applies while the CAN module is in
	// simulation mode (live data off); see PressureModule.SetSimValue.
	Values map[string]float64 `json:"values"`
}

func (w *WebServer) handleCanSim(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req canSimRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(rw, "invalid request body", http.StatusBadRequest)
		return
	}
	for name, bar := range req.Values {
		s, ok := ParseSensorInput(name)
		if !ok {
			http.Error(rw, fmt.Sprintf("unknown sensor %q", name), http.StatusBadRequest)
			return
		}
		if err := w.pm.SetSimValue(s, bar); err != nil {
			http.Error(rw, err.Error(), http.StatusBadRequest)
			return
		}
	}
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(w.pm.Status())
}

func (w *WebServer) handleTempStatus(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(w.tm.Status())
}

type tempSimRequest struct {
	// Values maps sensor name (SensorInput.String(), e.g. "TankTemp") to
	// the °C value to set. Only applies while the temperature module is
	// in simulation mode (live data off); see TemperatureModule.SetSimValue.
	Values map[string]float64 `json:"values"`
}

func (w *WebServer) handleTempSim(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req tempSimRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(rw, "invalid request body", http.StatusBadRequest)
		return
	}
	for name, c := range req.Values {
		s, ok := ParseSensorInput(name)
		if !ok {
			http.Error(rw, fmt.Sprintf("unknown sensor %q", name), http.StatusBadRequest)
			return
		}
		if err := w.tm.SetSimValue(s, c); err != nil {
			http.Error(rw, err.Error(), http.StatusBadRequest)
			return
		}
	}
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(w.tm.Status())
}

func (w *WebServer) handleDigitalStatus(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(w.dim.Status())
}

type digitalSimRequest struct {
	// Values maps signal name (DigitalInput.String(), e.g. "Motor1Powered")
	// to the level to set (true = high/1, false = low/0). Only applies
	// while the digital input module is in simulation mode (live data
	// off); see DigitalInputModule.SetSimValue.
	Values map[string]bool `json:"values"`
}

func (w *WebServer) handleDigitalSim(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req digitalSimRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(rw, "invalid request body", http.StatusBadRequest)
		return
	}
	for name, level := range req.Values {
		d, ok := ParseDigitalInput(name)
		if !ok {
			http.Error(rw, fmt.Sprintf("unknown digital input %q", name), http.StatusBadRequest)
			return
		}
		if err := w.dim.SetSimValue(d, level); err != nil {
			http.Error(rw, err.Error(), http.StatusBadRequest)
			return
		}
	}
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(w.dim.Status())
}

func (w *WebServer) handleFlowStatus(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(w.fm.Status())
}

// handleFlowReset zeroes the flow sensor's totalizer on demand - a
// staff diagnostic action, useful for testing without starting a whole
// fill job (Controller.Start already does this automatically at the
// start of every job). No-op (still 200 OK) if no sensor is
// configured/active - matches ResetTotal's own "handled" semantics.
func (w *WebServer) handleFlowReset(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if _, err := w.fm.ResetTotal(); err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(w.fm.Status())
}

type changePasswordRequest struct {
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
}

// handleChangePassword lets an already-authenticated admin rotate the
// shared login from the admin screen, without SSH access. Requiring the
// current password again (on top of requireAuth already having checked
// it for this request) guards against an unlocked, unattended browser
// tab being used to lock everyone else out. Takes effect immediately for
// new requests; a browser tab with the old password already cached will
// only notice on its next 401 (see AdminAuth.SetPassword).
func (w *WebServer) handleChangePassword(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req changePasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(rw, "invalid request body", http.StatusBadRequest)
		return
	}
	if subtle.ConstantTimeCompare([]byte(req.CurrentPassword), []byte(w.auth.Password())) != 1 {
		http.Error(rw, "current password is incorrect", http.StatusBadRequest)
		return
	}
	if err := w.auth.SetPassword(req.NewPassword); err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	rw.WriteHeader(http.StatusOK)
}

type lookupCodeRequest struct {
	Code string `json:"code"`
}

type lookupCodeResponse struct {
	Valid bool   `json:"valid"`
	Name  string `json:"name,omitempty"`
}

// handleLookupCode lets the customer screen show a welcome message as
// soon as a recognised code is entered - deliberately returns only that
// one code's name (never the list), so it's safe to leave open to the
// kiosk without an admin login, the same as status/start/stop.
func (w *WebServer) handleLookupCode(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req lookupCodeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(rw, "invalid request body", http.StatusBadRequest)
		return
	}
	var resp lookupCodeResponse
	if w.validCodes != nil {
		if name, ok := w.validCodes.Lookup(req.Code); ok {
			resp.Valid = true
			resp.Name = name
		}
	}
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(resp)
}

// handleRecords drains the dispense log: returns every completed fill
// recorded since the last call, and clears the file - see
// DispenseLog.DrainAll. Intended for a billing/accounting system to
// poll periodically.
func (w *WebServer) handleRecords(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(rw, "GET required", http.StatusMethodNotAllowed)
		return
	}
	records, err := w.dispenseLog.DrainAll()
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(struct {
		Records []DispenseRecord `json:"records"`
	}{Records: records})
}

type codesResponse struct {
	Codes []CodeEntry `json:"codes"`
}

// handleAdminCodes serves the admin screen's customer-code allow-list
// editor: GET the current code/name pairs, POST to validate and replace
// the whole list.
func (w *WebServer) handleAdminCodes(rw http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(codesResponse{Codes: w.validCodes.List()})

	case http.MethodPost:
		var req codesResponse
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(rw, "invalid request body", http.StatusBadRequest)
			return
		}
		if err := w.validCodes.SetAll(req.Codes); err != nil {
			http.Error(rw, err.Error(), http.StatusBadRequest)
			return
		}
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(codesResponse{Codes: w.validCodes.List()})

	default:
		http.Error(rw, "GET or POST required", http.StatusMethodNotAllowed)
	}
}

const (
	defaultHistoryLimit = 10000
	maxHistoryLimit     = 100000
)

type historyChannelsResponse struct {
	Readings []string `json:"readings"`
	Valves   []string `json:"valves"`
}

// handleHistoryChannels lists every valid channel name for the
// readings/valves history endpoints below, so a consumer never has to
// hardcode them - generated from the same enumerations the
// diagnostics page uses (see allPressureSensors etc. in controller.go).
func (w *WebServer) handleHistoryChannels(rw http.ResponseWriter, r *http.Request) {
	resp := historyChannelsResponse{}
	for _, s := range allPressureSensors {
		resp.Readings = append(resp.Readings, s.String())
	}
	for _, s := range allTempSensors {
		resp.Readings = append(resp.Readings, s.String())
	}
	for _, v := range allRelays {
		resp.Valves = append(resp.Valves, v.String())
	}
	for _, d := range allDigitalInputs {
		resp.Valves = append(resp.Valves, d.String())
	}
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(resp)
}

// parseHistoryRange reads the shared from/to/limit query parameters
// used by both history query endpoints below: from defaults to 24h
// ago, to defaults to now, limit defaults to defaultHistoryLimit and is
// capped at maxHistoryLimit.
func parseHistoryRange(r *http.Request) (from, to time.Time, limit int, err error) {
	now := time.Now()
	from, to = now.Add(-24*time.Hour), now
	if s := r.URL.Query().Get("from"); s != "" {
		from, err = time.Parse(time.RFC3339, s)
		if err != nil {
			return from, to, limit, fmt.Errorf("invalid from: %w", err)
		}
	}
	if s := r.URL.Query().Get("to"); s != "" {
		to, err = time.Parse(time.RFC3339, s)
		if err != nil {
			return from, to, limit, fmt.Errorf("invalid to: %w", err)
		}
	}
	limit = defaultHistoryLimit
	if s := r.URL.Query().Get("limit"); s != "" {
		limit, err = strconv.Atoi(s)
		if err != nil || limit <= 0 {
			return from, to, limit, fmt.Errorf("invalid limit %q", s)
		}
	}
	if limit > maxHistoryLimit {
		limit = maxHistoryLimit
	}
	return from, to, limit, nil
}

type historyReadingsResponse struct {
	Channel string         `json:"channel"`
	Points  []ReadingPoint `json:"points"`
}

// handleHistoryReadings serves logged pressure/temperature history for
// one channel over a time range - see HistoryStore.QueryReadings.
func (w *WebServer) handleHistoryReadings(rw http.ResponseWriter, r *http.Request) {
	channel := r.URL.Query().Get("channel")
	if _, ok := ParseSensorInput(channel); !ok {
		http.Error(rw, fmt.Sprintf("unknown reading channel %q", channel), http.StatusBadRequest)
		return
	}
	from, to, limit, err := parseHistoryRange(r)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	points, err := w.history.QueryReadings(channel, from, to, limit)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(historyReadingsResponse{Channel: channel, Points: points})
}

type historyValvesResponse struct {
	Channel string       `json:"channel"`
	Points  []StatePoint `json:"points"`
}

// handleHistoryValves serves logged valve/relay/digital-input state
// history for one channel over a time range - see
// HistoryStore.QueryValveStates.
func (w *WebServer) handleHistoryValves(rw http.ResponseWriter, r *http.Request) {
	channel := r.URL.Query().Get("channel")
	_, isRelay := ParseRelayOutput(channel)
	_, isDigital := ParseDigitalInput(channel)
	if !isRelay && !isDigital {
		http.Error(rw, fmt.Sprintf("unknown valve channel %q", channel), http.StatusBadRequest)
		return
	}
	from, to, limit, err := parseHistoryRange(r)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	points, err := w.history.QueryValveStates(channel, from, to, limit)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(historyValvesResponse{Channel: channel, Points: points})
}

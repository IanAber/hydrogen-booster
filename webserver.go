package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
)

type WebServer struct {
	ctrl        *Controller
	live        *LiveConfig
	pm          *PressureModule
	tm          *TemperatureModule
	listenAddr  string
	configPath  string
	webDir      string
	auth        *AdminAuth
	dispenseLog *DispenseLog
	validCodes  *ValidCodes
}

func NewWebServer(ctrl *Controller, live *LiveConfig, pm *PressureModule, tm *TemperatureModule, listenAddr, configPath, webDir string, auth *AdminAuth, dispenseLog *DispenseLog, validCodes *ValidCodes) *WebServer {
	return &WebServer{
		ctrl: ctrl, live: live, pm: pm, tm: tm,
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

// ListenAndServe wires up routes in two access tiers:
//
//   - Public-from-anywhere: the customer screen (index.html) and the
//     job-control API it needs (status/start/stop), plus shared static
//     assets (i18n.js, images/*) every page - including the staff ones -
//     loads.
//   - Public-from-localhost-only, auth required otherwise: the same
//     customer screen's API when reached over the network rather than
//     from the on-site kiosk. This is requireAuthUnlessLocal on "/" and
//     the job-control endpoints - see auth.go.
//   - Staff-only, always auth required regardless of origin: the
//     operator/admin/diagnostics pages and every API endpoint that lets
//     you reconfigure the system, read raw diagnostics, or adjust
//     simulated sensor values.
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
	mux.HandleFunc("/api/diagnostics", w.requireAuth(w.handleDiagnostics))
	mux.HandleFunc("/api/clearfault", w.requireAuth(w.handleClearFault))
	mux.HandleFunc("/api/config", w.requireAuth(w.handleConfig))
	mux.HandleFunc("/api/can/status", w.requireAuth(w.handleCanStatus))
	mux.HandleFunc("/api/can/sim", w.requireAuth(w.handleCanSim))
	mux.HandleFunc("/api/temp/status", w.requireAuth(w.handleTempStatus))
	mux.HandleFunc("/api/temp/sim", w.requireAuth(w.handleTempSim))
	mux.HandleFunc("/api/admin/password", w.requireAuth(w.handleChangePassword))
	mux.HandleFunc("/api/records", w.requireAuth(w.handleRecords))
	mux.HandleFunc("/api/admin/codes", w.requireAuth(w.handleAdminCodes))
	mux.HandleFunc("/docs.html", w.requireAuth(w.serveStatic("docs.html")))
	mux.HandleFunc("/api/docs", w.requireAuth(w.handleAPIDocs))

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

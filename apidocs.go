package main

import (
	"encoding/json"
	"net/http"
)

// apiEndpointDoc describes one HTTP endpoint for /api/docs and
// docs.html - the single source of truth for both, so the two never
// drift out of sync with each other (they can still drift from the
// handlers themselves if a route changes without updating this list).
type apiEndpointDoc struct {
	Method   string `json:"method"`
	Path     string `json:"path"`
	Auth     string `json:"auth"`
	Summary  string `json:"summary"`
	Request  string `json:"request,omitempty"`
	Response string `json:"response,omitempty"`
}

const (
	authPublicLocal = "public locally, staff login required remotely"
	authStaff       = "staff login required"
)

var apiDocs = []apiEndpointDoc{
	{
		Method: "GET", Path: "/api/status", Auth: authPublicLocal,
		Summary:  "Current job/sensor status, polled continuously by the customer and operator screens.",
		Response: "phase, running, nozzle, targetPressureBar, targetMassKg, outputPressureBar, outputTempC, tankPressureBar, hydOilTempC, deliveredMassKg, stage1, stage2, ventOpen, faults, startedAt, elapsedSec",
	},
	{
		Method: "POST", Path: "/api/start", Auth: authPublicLocal,
		Summary: "Start a fill job.",
		Request: `{"targetPressureBar":350, "targetMassKg":1, "nozzle":350, "customerCode":"1234"}`,
	},
	{
		Method: "POST", Path: "/api/stop", Auth: authPublicLocal,
		Summary: "Abort the running job. No request body.",
	},
	{
		Method: "POST", Path: "/api/lookup-code", Auth: authPublicLocal,
		Summary:  "Look up one customer code, for the customer screen's welcome message.",
		Request:  `{"code":"1234"}`,
		Response: `{"valid":true, "name":"Jane Doe"}`,
	},
	{
		Method: "GET", Path: "/api/diagnostics", Auth: authStaff,
		Summary:  "Raw valve/sensor readings, bypassing job/phase state - for commissioning and wiring checks.",
		Response: `{"valves":[{"name":"Valve1A(dir)","on":false,"ok":true}], "sensors":[{"name":"OutputTemp","kind":"temp","value":30.1,"ok":true}]}`,
	},
	{
		Method: "POST", Path: "/api/clearfault", Auth: authStaff,
		Summary: "Acknowledge and clear a fault, returning to idle. No request body.",
	},
	{
		Method: "GET, POST", Path: "/api/config", Auth: authStaff,
		Summary:  "GET the current editable configuration; POST a new one (applies immediately, add ?persist=true to also save to the config file on disk).",
		Request:  "POST body: a full ConfigDTO (see admin.html for every field)",
		Response: `{"config": {...ConfigDTO...}, "configFilePath":"/var/hydrogen-booster/config.json", "saved":false, "saveError":""}`,
	},
	{
		Method: "GET", Path: "/api/can/status", Auth: authStaff,
		Summary: "CAN pressure-module channel status (assignment, live/sim mode, current values).",
	},
	{
		Method: "POST", Path: "/api/can/sim", Auth: authStaff,
		Summary: "Set simulated pressure readings (only while the CAN module is in simulation mode).",
		Request: `{"values":{"TankPressure":200}}`,
	},
	{
		Method: "GET", Path: "/api/temp/status", Auth: authStaff,
		Summary: "1-Wire temperature-module channel status (assignment, live/sim mode, current values).",
	},
	{
		Method: "POST", Path: "/api/temp/sim", Auth: authStaff,
		Summary: "Set simulated temperature readings (only while the temperature module is in simulation mode).",
		Request: `{"values":{"OutputTemp":30}}`,
	},
	{
		Method: "POST", Path: "/api/admin/password", Auth: authStaff,
		Summary: "Change the admin password.",
		Request: `{"currentPassword":"...", "newPassword":"..."}`,
	},
	{
		Method: "GET", Path: "/api/records", Auth: authStaff,
		Summary:  "Drain the dispense log: returns every completed fill since the last call, and clears it. Intended for a billing/accounting system to poll.",
		Response: `{"records":[{"timestamp":"2026-09-18T12:00:00Z", "customerCode":"1234", "deliveredMassKg":1.234}]}`,
	},
	{
		Method: "GET, POST", Path: "/api/admin/codes", Auth: authStaff,
		Summary:  "GET the customer-code allow-list, or POST to validate and replace it entirely.",
		Request:  `{"codes":[{"code":"1234","name":"Jane Doe"}]}`,
		Response: `{"codes":[{"code":"1234","name":"Jane Doe"}]}`,
	},
	{
		Method: "GET", Path: "/api/docs", Auth: authStaff,
		Summary:  "This list, as JSON.",
		Response: "an array of apiEndpointDoc: method, path, auth, summary, request, response",
	},
}

func (w *WebServer) handleAPIDocs(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(rw, "GET required", http.StatusMethodNotAllowed)
		return
	}
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(apiDocs)
}

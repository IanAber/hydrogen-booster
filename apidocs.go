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
		Response: "phase, running, nozzle, targetPressureBar, targetMassKg, outputPressureBar, outputTempC, tankPressureBar, hydOilTempC, deliveredMassKg, stage1, stage2, ventOpen, faults, faultDetails (each fault with code/params for translation), startedAt, elapsedSec, manualMode, nozzles {has700Bar, sensorsEnabled, detected (350/700, 0 = none), problem}",
	},
	{
		Method: "POST", Path: "/api/start", Auth: authPublicLocal,
		Summary: "Start a fill job. nozzle must be 350 or 700 (700 only if the system has a 700 bar nozzle). With nozzle proximity sensors enabled, the lifted nozzle is used - nozzle may be omitted, and the request is refused unless exactly one nozzle is lifted (or if it names a different one).",
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
		Response: `{"valves":[{"name":"Valve1A(dir)","on":false,"ok":true}], "sensors":[{"name":"OutputTemp","kind":"temp","value":30.1,"ok":true}], "digitalInputs":[{"name":"HydraulicOilLevel","high":false,"ok":true}]}`,
	},
	{
		Method: "POST", Path: "/api/clearfault", Auth: authStaff,
		Summary: "Acknowledge and clear a fault, returning to idle. No request body.",
	},
	{
		Method: "POST", Path: "/api/manual/enable", Auth: authStaff,
		Summary: "Enable or disable manual piston control. Enabling is refused while a job is running; both enabling and disabling return every manually-controlled output to its safe default (relief valves B/D open, direction valves A/C off, pumps off).",
		Request: `{"enable":true}`,
	},
	{
		Method: "POST", Path: "/api/manual/pump", Auth: authStaff,
		Summary: "Turn one stage's hydraulic pump motor on or off. Only permitted while manual mode is enabled.",
		Request: `{"stage":1, "on":true}`,
	},
	{
		Method: "POST", Path: "/api/manual/jog", Auth: authStaff,
		Summary: "Drive one stage's cylinder while its Up/Down button is held: active=true sets the direction valve, closing the relief valve immediately if that's already the current direction or after a ReverseDelay settling pause if it isn't; active=false re-opens the relief valve right away and cancels any pending settling pause. Only permitted while manual mode is enabled.",
		Request: `{"stage":1, "direction":"up", "active":true}`,
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
		Method: "GET", Path: "/api/digital/status", Auth: authStaff,
		Summary: "Digital input channel status (assignment, live/sim mode, current level).",
	},
	{
		Method: "POST", Path: "/api/digital/sim", Auth: authStaff,
		Summary: "Set simulated digital input levels (only while the digital input module is in simulation mode).",
		Request: `{"values":{"HydraulicOilLevel":false}}`,
	},
	{
		Method: "GET", Path: "/api/flow/status", Auth: authStaff,
		Summary:  "Mass flow sensor status: selected sensor type, whether it's currently active, current totalizer reading (kg), comms status.",
		Response: `{"sensorType":"aalborg_xfm", "active":true, "lastUpdate":"2026-09-18T12:00:00Z", "totalKg":1.234, "ok":true}`,
	},
	{
		Method: "POST", Path: "/api/flow/reset", Auth: authStaff,
		Summary: "Zero the flow sensor's totalizer on demand (Controller already does this automatically at the start of every fill job). No-op if no sensor is configured/active. No request body.",
	},
	{
		Method: "POST", Path: "/api/admin/password", Auth: authStaff,
		Summary: "Change the admin password.",
		Request: `{"currentPassword":"...", "newPassword":"..."}`,
	},
	{
		Method: "GET", Path: "/api/records", Auth: authStaff,
		Summary:  "Completed fills from the dispense database, oldest first - customer ID and name, never the login code. Records are kept permanently, so the same range can be fetched again.",
		Request:  "query params (all optional): from, to - RFC3339 time or YYYY-MM-DD date (local midnight), from inclusive, to exclusive; customerId",
		Response: `{"records":[{"id":1, "timestamp":"2026-09-18T12:00:00Z", "customerId":"ACME-001", "customerName":"Acme Haulage", "deliveredMassKg":1.234}]}`,
	},
	{
		Method: "GET, POST", Path: "/api/admin/codes", Auth: authStaff,
		Summary:  "GET the customer-code allow-list, or POST to validate and replace it entirely. Each login code maps to a unique customer ID (up to 32 letters, digits, '.', '_' or '-') and name.",
		Request:  `{"codes":[{"code":"1234","customerId":"ACME-001","name":"Jane Doe"}]}`,
		Response: `{"codes":[{"code":"1234","customerId":"ACME-001","name":"Jane Doe"}]}`,
	},
	{
		Method: "GET", Path: "/api/docs", Auth: authStaff,
		Summary:  "This list, as JSON.",
		Response: "an array of apiEndpointDoc: method, path, auth, summary, request, response",
	},
	{
		Method: "GET", Path: "/api/history/channels", Auth: authStaff,
		Summary:  "Valid channel names for /api/history/readings and /api/history/valves.",
		Response: `{"readings":["Hyd1Pressure","OutputTemp",...], "valves":["Valve1A(dir)","Motor1Powered",...]}`,
	},
	{
		Method: "GET", Path: "/api/history/readings", Auth: authStaff,
		Summary:  "Logged pressure/temperature history for one channel, sampled on a fixed interval (see -history-sample-interval).",
		Request:  "query params: channel (required, see /api/history/channels), from/to (RFC3339, default last 24h), limit (default 10000, max 100000)",
		Response: `{"channel":"Hyd1Pressure", "points":[{"ts":"2026-09-21T12:00:00Z","value":182.4}]}`,
	},
	{
		Method: "GET", Path: "/api/history/valves", Auth: authStaff,
		Summary:  "Logged valve/relay/digital-input state history for one channel, recorded only on state change; the first point returned carries forward whatever state was already in effect at the start of the requested window.",
		Request:  "query params: channel (required, see /api/history/channels), from/to (RFC3339, default last 24h), limit (default 10000, max 100000)",
		Response: `{"channel":"Pump1", "points":[{"ts":"2026-09-21T12:00:00Z","state":true}]}`,
	},
	{
		Method: "GET", Path: "/api/log", Auth: authStaff,
		Summary:  "This service's systemd journal entries (journalctl -u hydrogen-booster), oldest first, streamed as plain text - one entry per line. Only available on the device itself (503 where journalctl isn't installed).",
		Request:  "query params: range (all|today|24h|7d, default all), problems=1 for warning priority and above only",
		Response: "text/plain lines: <local time>\t<syslog priority 0-7, 3=error 4=warning 6=info>\t<message>",
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

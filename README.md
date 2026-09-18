# Hydrogen Booster Control

A Go control program for a two-stage hydraulic hydrogen booster (Stage 1 =
cylinders A/B, Stage 2 = cylinders C/D, each driven by its own hydraulic
ram), with a built-in web front end. Everything runs from a single binary
— the web UI is compiled into it via `go:embed`, no external files or
internet access needed at runtime.

## Running it

```
go run .
```

Then open `http://localhost:8080`. By default the program drives a
**simulated** hydraulic/hydrogen circuit (`SimulatedIO` in `io.go`) so you
can exercise the whole sequence — including the 700 bar vent cycle — with
no hardware attached.

## How the control logic maps to your description

- **`stage.go`** implements one reciprocating hydraulic ram (used once for
  Stage 1/A-B, once for Stage 2/C-D). Each stroke ends, and the direction
  valve (`1A`/`2A`) reverses, when **either**:
  - hydraulic pressure spikes (rises by a configurable amount within a
    short window) — the signature of the ram stalling against the gas
    column at the end of a stroke, or
  - a configurable max stroke time elapses first (a safety/fallback
    timeout, e.g. if a spike is missed).

  On every reversal, the secondary valve (`1B`/`2B`) is energised for
  ~500 ms **coincident with** the direction change, exactly as specified,
  then de-energised again (`Stage.pulseSecondary`).

- **Synchronisation** (`controller.go`, `Controller.stage2.AllowReverse`):
  Stage 2 is downstream of Stage 1 (it receives Stage 1's output), so it's
  held (direction valve left as-is) if it would get more than
  `Safety.MaxSyncStrokeLead` strokes ahead of Stage 1, and released again
  as soon as Stage 1 catches up. Tune `MaxSyncStrokeLead` and the per-stage
  stroke-time/spike settings to match the real machine's timing.

- **Job sequencing** (`Controller.Start` / `superviseJob`): operator sets a
  target output pressure, a target mass (kg), and picks a 350 or 700 bar
  nozzle. Both stages run until *either* target is reached, then stop.
  If the 700 bar nozzle was selected, the vent valve is opened for a
  configurable duration to bleed the delivery hose, then closed — this
  step is skipped for the 350 bar nozzle, as specified.

- **Safety interlocks** (`Controller.superviseSafety`): both hydraulic
  circuits' pressure, the hydrogen output pressure, and all temperature
  sensors are checked against configurable limits every poll cycle; a
  breach stops both stages, closes the vent valve, and latches a fault
  that the operator must acknowledge (`Clear Fault` button / `/api/clearfault`)
  before starting another job.

- **Mass estimate** (`gas.go`): there's no flow meter in the sensor list
  you described, so delivered mass is estimated from how much the mass of
  gas *in the storage tank bank* has dropped (pressure + temperature +
  known tank volume, via the ideal gas law with a rough hydrogen
  compressibility correction). **This is an HMI-quality estimate, not a
  custody-transfer or fuelling-protocol grade measurement** — see the
  comment at the top of `gas.go`.

## Persisting configuration to a file

Timing and pressure/temperature settings (everything on the admin
screen) can be saved to a human-readable JSON file so they survive a
restart:

```
go run . -config /path/to/config.json
```

`-config` defaults to `config.json` in the working directory. On startup
the program loads that file if it exists (logging `Loaded configuration
from ...`) and merges it onto the built-in defaults (`DefaultConfig()`
in `config.go`, which also still supplies the relay/sensor wiring - see
below); if the file doesn't exist yet, it just logs that it's using
built-in defaults and carries on; on the CM5 board you'd typically run
this from a systemd unit with an absolute `-config` path, e.g.
`/etc/hydrogen-booster/config.json`, so it survives across working
directories/deployments.

The admin screen (`/admin.html`) has two separate actions, deliberately
kept apart:

- **Apply** — takes effect immediately (in memory only). Good for trying
  a setting live without committing to it.
- **Apply & Save to File** — does the same, and also writes the file at
  the `-config` path as indented, human-readable JSON (not compressed/
  minified), so it's still in effect the next time the program starts.

The file is intentionally just the editable settings (`ConfigDTO`) - the
same shape the admin screen edits - so it's easy to read or hand-edit on
the device, e.g.:

```json
{
  "pollPeriodMs": 100,
  "stage1": {
    "name": "Stage 1 (A/B)",
    "minStrokeTimeMs": 300,
    "maxStrokeTimeMs": 6000,
    "spikeDeltaBar": 15,
    "spikeWindowMs": 250,
    "pulseDurationMs": 500
  },
  "...": "stage2, safety, tank, vent follow the same shape"
}
```

## Admin / configuration screen

`http://localhost:8080/admin.html` (linked from the main dashboard) lets an
operator or engineer adjust, on the fly, with no restart:

- Per-stage **min/max stroke time**, **pressure-spike threshold and
  window**, and **pulse valve duration** (Stage 1 and Stage 2 separately)
- **Safety limits**: max hydraulic pressure, max hydrogen pressure, max
  temperature, sensor max age, max synchronisation stroke lead
- **Tank** bank volume/nominal pressure/temperature (used for the mass
  estimate)
- **Vent duration** for the 700 bar completion sequence
- The control-loop **poll period**

Backed by `GET`/`POST /api/config` (`liveconfig.go`, `configfile.go`,
`webserver.go`), which validate (e.g. max stroke time must exceed min,
limits must be positive) before applying. A change takes effect on the
very next control-loop tick — typically well under the poll period,
~100ms by default. See "Persisting configuration to a file" above for
how Apply vs. Apply & Save to File differ.

## Pressure input module (CAN bus)

The first real sensor driver: a 6-channel 4-20mA loop pressure input
module talking over isolated CAN bus. Protocol (confirmed against the
real hardware):

```
request:  cansend can0 101#020006      (function 0x02, offset 0x00, count 0x06)
response: two 8-byte frames on ID 0x001, each:
            byte0        = echoed function code
            byte1        = channel offset of the first value in this frame
            bytes2-7     = up to three 16-bit raw values, big-endian (high byte first)
```

- **`canframe.go`** — the protocol itself (frame build/parse), pure Go with no hardware dependency, unit-tested against the exact bytes from the real module.
- **`cansocket_linux.go`** — the real driver: a raw Linux SocketCAN (`AF_CAN`/`CAN_RAW`) socket, stdlib only (`syscall`/`net`, no external CAN library or `golang.org/x/sys` dependency to fetch). Expects the interface already up at the configured bitrate (e.g. `ip link set can0 up type can bitrate 500000`, normally a boot-time systemd/netplan unit on the CM5 image, not this program's job).
- **`pressuremodule.go`** — polls the module, applies each channel's calibration, and implements the live/simulation switch described below.
- **`compositeio.go`** — layers the pressure module over the existing simulated IO, so anything not assigned to a CAN channel is still served by `SimulatedIO`, and everything else in the codebase (`Stage`, `Controller`, the web UI) doesn't know or care which sensors are real yet.

### Channel assignment and calibration

Each of the 6 physical channels can be, from the admin screen:
- **Enabled/disabled**
- **Assigned** to one of the five pressure readings the system uses (`Hyd1Pressure`, `Hyd2Pressure`, `TankPressure`, `InterStagePressure`, `OutputPressure`) - assigning the same reading to two enabled channels is rejected
- **Calibrated** with a two-point linear fit: a low raw count + the pressure (bar) it corresponds to, and the same for a high point. To calibrate a channel: apply two known pressures to the sensor (e.g. atmospheric and a known test pressure), read the raw counts shown live on the admin screen at each, and enter both pairs. Raw readings outside the two calibration points still extrapolate linearly rather than clamping, so a miscalibrated or disconnected sensor shows an implausible value instead of a falsely-plausible clamped one.

The defaults in `DefaultConfig()` (`config.go`) are **placeholders** (a straight 0-65535 raw count mapped across an assumed full-scale bar figure per channel) - every channel needs real calibration against the actual module before its readings mean anything.

### Live data / simulation switch

The admin screen's **Live data feed** checkbox controls where every assigned channel's value comes from:

- **On** - values come from the CAN bus (`can0` is opened and polled at `pollPeriodMs`; comms status and any error show live on the admin screen).
- **Off** - the CAN socket is closed (nothing touches the bus), and the moment it's switched off, the last live reading for each assigned channel is copied once into an editable simulated value. Those values then show as number inputs on the admin screen's "Live Readings / Simulator" panel, adjustable in real time (each edit posts to `POST /api/can/sim`, debounced, independent of the main config form) to exercise the rest of the control system - stage reversal, safety interlocks, job completion - without any hardware attached.

This is also why the module defaults to simulation mode: it never touches `can0` until an operator has confirmed wiring and calibration and switched it on deliberately.

### What I could and couldn't verify here

I don't have real CAN hardware (or even a loadable `vcan` kernel module) in this environment, so I verified what's possible without it:
- The full request/response protocol logic, big-endian decoding, and calibration math are unit-tested (`canframe_test.go`, `pressuremodule_test.go`) against the exact bytes from your `cansend`/response example.
- The live/simulation switch, its snapshot-on-transition behavior, and the config file round-trip are also unit-tested.
- End-to-end, I confirmed (against the simulated IO, with the CAN interface absent) that: the module correctly reports a comms fault rather than crashing when `can0` doesn't exist; switching live on/off doesn't spam retries or leak the socket; and a simulated value set via the admin screen correctly flows through to the rest of the system (`/api/status`) via `CompositeIO`.
- What I could **not** verify here is an actual CAN frame going out over a wire and a real response coming back - `cansocket_linux.go`'s socket/bind/frame-layout code follows the standard, well-documented SocketCAN ABI, but its first real test will be on your CM5 hardware. Worth a quick sanity check there before relying on it: run the program with `-config` pointing somewhere writable, switch Live on from the admin screen, and confirm the "Pressure module: opened CAN interface can0" log line appears and the readings match `candump can0` alongside it.

## Roadmap: web-based relay/IO configuration

Relay wiring (which physical output each `RelayOutput` like `Valve1A`
refers to) is not yet editable from the web UI; for now it's set in code
(`DefaultConfig()` in `config.go`) and requires a rebuild/restart to
change. The CAN pressure module above is the first sensor to get
web-based channel assignment; the same pattern (a small per-device
config struct + DTO + admin card) is the template for wiring up relay
configuration and any further real sensor drivers as they're supplied.

## Wiring it to real hardware

Everything above the `IO` interface (`io.go`) is hardware-agnostic. This
is written to run on a custom Raspberry Pi CM5 based board. The CAN
pressure module (above) is the first real driver, wired in via
`CompositeIO`; everything else - relays and any pressure/temperature
sensor not assigned to the CAN module - is still simulated (`SimulatedIO`)
so the rest of the system - sequencing, synchronisation, safety
interlocks, the web UI, config persistence - can be developed and
demonstrated ahead of the remaining hardware drivers. To add another real
driver the same way:

1. Write a new type implementing `IO` (`SetRelay`, `RelayState`,
   `ReadPressureBar`, `ReadTempC`, `Close`) that talks to your actual I/O
   — e.g. a Modbus TCP/RTU remote I/O rack, EtherCAT slave, or direct
   GPIO, depending on what's on the skid — or follow the CAN module's
   pattern (a thin `CanTransport`-style interface plus a small
   `*Module` type) if it's simpler to layer it the same way instead of
   replacing `SimulatedIO` wholesale.
2. In `main.go`, wire it in - either replacing `NewSimulatedIO()`
   entirely, or layering it into `CompositeIO` alongside the pressure
   module the way `PressureModule` is layered in now.
3. Re-tune every value in `DefaultConfig()` (`config.go`) against the real
   machine: stroke timeout, pressure-spike threshold/window, pulse
   duration, and — most importantly — the safety limits. None of the
   defaults here are validated against a real machine.

## Safety notes (please read before connecting to real plant)

This program is a **process controller**, not a safety system. For a
compressed-hydrogen skid it should sit *behind*, not *instead of*, proper
hardwired protection:

- Pressure relief valves / burst discs sized for each stage and for the
  final delivery line, set below the equipment's certified rating.
- An emergency-stop circuit that de-energises the hydraulic power unit and
  all solenoid valves **independent of this software** (i.e. it must work
  even if the PC/PLC running this program has crashed or hung).
- Hydrogen leak detection and forced ventilation appropriate to the
  installation area, with hardwired shutdown on detection.
- Electrical equipment rated for the hazardous area classification of
  where it's installed (e.g. ATEX/IECEx if applicable).
- If this is a vehicle-refuelling application, review SAE J2601 / ISO
  19880-1 — the actual fuelling algorithm (temperature-compensated fill
  rate and final pressure) is more involved than "stop at a pressure or
  mass setpoint," and worth building against a certified reference rather
  than this simulation.

Have the full design (mechanical, hydraulic, electrical and this control
logic) reviewed by a qualified process/pressure-equipment engineer before
it goes anywhere near real hydrogen.

## Files

| File           | Purpose                                               |
|----------------|--------------------------------------------------------|
| `main.go`      | Entry point, wires IO -> Controller -> WebServer        |
| `config.go`    | All tunable parameters                                 |
| `io.go`        | Hardware abstraction + `SimulatedIO`                    |
| `stage.go`     | Per-ram reciprocating state machine                      |
| `controller.go`| Synchronisation, safety interlocks, job sequencing        |
| `gas.go`       | Pressure/temperature -> mass estimate                    |
| `configfile.go`| Load/save the editable config as human-readable JSON     |
| `canframe.go`  | CAN pressure module protocol (frame build/parse) - pure, hardware-free |
| `cansocket_linux.go` | Real SocketCAN driver (Linux/CM5)                  |
| `cansocket_other.go` | Stub so non-Linux builds still compile             |
| `pressuremodule.go`  | Polling, calibration, and the live/simulation switch |
| `compositeio.go`     | Layers the pressure module over the simulated base IO |
| `webserver.go` | REST API (`/api/status`, `/api/start`, `/api/stop`, `/api/clearfault`, `/api/config`, `/api/can/status`, `/api/can/sim`) |
| `web/index.html` | Front end (single embedded page, polls `/api/status` every 500 ms) |
| `web/admin.html` | Admin screen: timing/safety config, CAN module config, live readings/simulator |
| `*_test.go`    | Unit tests for the CAN protocol, calibration, and live/sim switch (no hardware needed) |

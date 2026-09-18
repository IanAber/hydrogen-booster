package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// LoadConfigFile reads a human-readable JSON config file - the same
// shape ConfigDTO/the admin screen uses, i.e. timing and pressure/
// temperature settings only, in plain millisecond integers rather than
// Go's compact-but-opaque Duration encoding - and merges it onto base
// (which supplies wiring: relay/sensor assignments, listen address).
//
// If the file does not exist this is not an error: ok is false and base
// is returned unchanged, so callers can fall back to built-in defaults
// on first run (before anything has ever been saved).
func LoadConfigFile(path string, base Config) (cfg Config, ok bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return base, false, nil
		}
		return base, false, fmt.Errorf("reading %s: %w", path, err)
	}
	var dto ConfigDTO
	if err := json.Unmarshal(data, &dto); err != nil {
		return base, false, fmt.Errorf("parsing %s: %w", path, err)
	}
	merged, err := FromDTO(base, dto)
	if err != nil {
		return base, false, fmt.Errorf("validating %s: %w", path, err)
	}
	return merged, true, nil
}

// SaveConfigFile writes the editable portion of cfg (see ConfigDTO) to
// path as indented, human-readable JSON - explicitly not compacted or
// compressed, so it can be inspected/edited by hand on the device (e.g.
// over SSH) as well as from the admin web screen.
func SaveConfigFile(path string, cfg Config) error {
	dto := ToDTO(cfg)
	data, err := json.MarshalIndent(dto, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding config: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

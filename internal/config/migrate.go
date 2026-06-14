package config

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Migrate parses raw JSON and returns a Config at CurrentVersion, applying any
// version-to-version upgrades in between. It does NOT validate the result —
// callers run Validate after migrating, keeping "make it current" and "is it
// usable" as distinct, independently testable steps.
//
// Rules:
//   - A missing or zero "version" is treated as version 1 (the first schema),
//     so the earliest configs, written before versioning existed, still load.
//   - A version newer than this build (CurrentVersion) is rejected: a newer
//     release may have introduced fields or semantics we cannot honor, and
//     silently downgrading would lose data.
//   - Unknown fields are rejected to catch typos and stale keys early.
//
// Migrations are applied one step at a time (v1->v2, then v2->v3, ...) so each
// upgrade is small and the chain composes for far-behind configs.
func Migrate(raw []byte) (Config, error) {
	// Peek at the version without committing to the full v2 shape, since an
	// older file may carry renamed or absent fields.
	var probe struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return Config{}, fmt.Errorf("config: parse version: %w", err)
	}
	v := probe.Version
	if v == 0 {
		v = 1 // pre-versioning configs are version 1 by definition.
	}
	if v > CurrentVersion {
		return Config{}, fmt.Errorf("config: version %d is newer than this build supports (max %d); upgrade NilCore",
			v, CurrentVersion)
	}

	// Walk the migration chain, rewriting the JSON one version at a time. Each
	// step takes raw bytes at version n and returns raw bytes at version n+1,
	// so the final decode targets the current Config shape exactly.
	cur := raw
	for v < CurrentVersion {
		step, ok := migrations[v]
		if !ok {
			return Config{}, fmt.Errorf("config: no migration from version %d", v)
		}
		next, err := step(cur)
		if err != nil {
			return Config{}, fmt.Errorf("config: migrate v%d->v%d: %w", v, v+1, err)
		}
		cur = next
		v++
	}

	cfg, err := decodeStrict(cur)
	if err != nil {
		return Config{}, fmt.Errorf("config: decode v%d: %w", CurrentVersion, err)
	}
	return cfg, nil
}

// migrations maps a source version to the function that upgrades its raw JSON to
// the next version. Add one entry per schema bump; the chain in Migrate does the
// rest.
var migrations = map[int]func([]byte) ([]byte, error){
	1: migrateV1toV2,
}

// migrateV1toV2 handles the v1->v2 schema change: the field once spelled
// "engine" was renamed to "executor". We read the loose v1 object, move the
// value across (without clobbering an explicit "executor" if one is present),
// drop the old key, and stamp the new version.
func migrateV1toV2(raw []byte) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse object: %w", err)
	}
	if engine, ok := m["engine"]; ok {
		if _, hasNew := m["executor"]; !hasNew {
			m["executor"] = engine
		}
		delete(m, "engine")
	}
	m["version"] = json.RawMessage(fmt.Sprintf("%d", 2))

	out, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("re-encode: %w", err)
	}
	return out, nil
}

// decodeStrict unmarshals current-version JSON into Config, rejecting unknown
// fields so a typo or a stale key surfaces as an error instead of being silently
// ignored.
func decodeStrict(raw []byte) (Config, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return Config{}, err
	}
	return c, nil
}

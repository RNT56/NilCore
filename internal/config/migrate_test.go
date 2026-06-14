package config

import (
	"strings"
	"testing"
)

func TestMigrateV1toV2(t *testing.T) {
	// A v1 config: note the renamed field "engine" (now "executor") and no
	// "max_steps" yet — migration moves the value across and stamps version 2.
	v1 := []byte(`{
		"version": 1,
		"engine": "codex",
		"runtime": "container",
		"model": "claude-sonnet-4",
		"max_steps": 25
	}`)

	got, err := Migrate(v1)
	if err != nil {
		t.Fatalf("Migrate v1: %v", err)
	}
	if got.Version != CurrentVersion {
		t.Fatalf("migrated Version = %d, want %d", got.Version, CurrentVersion)
	}
	// The load-bearing assertion: the renamed field carried over.
	if got.Executor != ExecutorCodex {
		t.Fatalf("migrated Executor = %q, want %q (from v1 \"engine\")", got.Executor, ExecutorCodex)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("migrated config should be valid: %v", err)
	}
}

func TestMigrateUnversionedTreatedAsV1(t *testing.T) {
	// No "version" key at all: the earliest configs predate versioning and are
	// version 1 by definition, so "engine" must still migrate.
	raw := []byte(`{"engine":"native","runtime":"none","model":"m","max_steps":10}`)
	got, err := Migrate(raw)
	if err != nil {
		t.Fatalf("Migrate unversioned: %v", err)
	}
	if got.Version != CurrentVersion {
		t.Fatalf("Version = %d, want %d", got.Version, CurrentVersion)
	}
	if got.Executor != ExecutorNative {
		t.Fatalf("Executor = %q, want %q", got.Executor, ExecutorNative)
	}
}

func TestMigrateCurrentVersionPassesThrough(t *testing.T) {
	in := Default()
	raw, err := in.marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := Migrate(raw)
	if err != nil {
		t.Fatalf("Migrate current: %v", err)
	}
	if got != in {
		t.Fatalf("pass-through mismatch: got=%+v want=%+v", got, in)
	}
}

func TestMigrateRejects(t *testing.T) {
	tests := []struct {
		name   string
		raw    string
		substr string
	}{
		{
			name:   "too new",
			raw:    `{"version": 999, "executor": "native", "runtime": "container", "model": "m", "max_steps": 5}`,
			substr: "newer than this build",
		},
		{
			name:   "malformed json",
			raw:    `{not json`,
			substr: "parse version",
		},
		{
			name:   "unknown field at current version",
			raw:    `{"version":2,"executor":"native","runtime":"container","model":"m","max_steps":5,"bogus":true}`,
			substr: "decode",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Migrate([]byte(tt.raw))
			if err == nil {
				t.Fatalf("Migrate(%q) = nil, want error", tt.raw)
			}
			if !strings.Contains(err.Error(), tt.substr) {
				t.Fatalf("Migrate error = %v, want substring %q", err, tt.substr)
			}
		})
	}
}

// When both the old and new field names appear during a v1->v2 migration, the
// explicit new field wins and the deprecated one is dropped silently.
func TestMigrateV1PrefersExplicitNewField(t *testing.T) {
	raw := []byte(`{"version":1,"engine":"codex","executor":"native","runtime":"none","model":"m","max_steps":1}`)
	got, err := Migrate(raw)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if got.Executor != ExecutorNative {
		t.Fatalf("Executor = %q, want %q (explicit new field wins)", got.Executor, ExecutorNative)
	}
}

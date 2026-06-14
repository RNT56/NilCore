package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDefaultValidates(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatalf("Default() must be valid, got: %v", err)
	}
	if Default().Version != CurrentVersion {
		t.Fatalf("Default().Version = %d, want %d", Default().Version, CurrentVersion)
	}
}

func TestValidate(t *testing.T) {
	base := Default()

	tests := []struct {
		name    string
		mutate  func(c *Config)
		wantErr bool
		// substr, when set, must appear in the error message so callers get a
		// specific, actionable diagnostic.
		substr string
	}{
		{
			name:    "valid default",
			mutate:  func(*Config) {},
			wantErr: false,
		},
		{
			name:    "empty executor",
			mutate:  func(c *Config) { c.Executor = "" },
			wantErr: true,
			substr:  "executor is empty",
		},
		{
			name:    "unknown executor",
			mutate:  func(c *Config) { c.Executor = "gpt-cli" },
			wantErr: true,
			substr:  `unknown executor "gpt-cli"`,
		},
		{
			name:    "empty runtime",
			mutate:  func(c *Config) { c.Runtime = "" },
			wantErr: true,
			substr:  "runtime is empty",
		},
		{
			name:    "unknown runtime",
			mutate:  func(c *Config) { c.Runtime = "vm" },
			wantErr: true,
			substr:  `unknown runtime "vm"`,
		},
		{
			name:    "empty model",
			mutate:  func(c *Config) { c.Model = "  " },
			wantErr: true,
			substr:  "model is empty",
		},
		{
			name:    "non-positive max steps",
			mutate:  func(c *Config) { c.MaxSteps = 0 },
			wantErr: true,
			substr:  "max_steps must be positive",
		},
		{
			name:    "wrong version",
			mutate:  func(c *Config) { c.Version = 1 },
			wantErr: true,
			substr:  "version 1 is not supported",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := base
			tt.mutate(&c)
			err := c.Validate()
			if tt.wantErr && err == nil {
				t.Fatalf("Validate() = nil, want error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
			if tt.substr != "" && (err == nil || !strings.Contains(err.Error(), tt.substr)) {
				t.Fatalf("Validate() error = %v, want substring %q", err, tt.substr)
			}
		})
	}
}

// Validate must not mutate the receiver: it is a verdict, not a normalizer.
func TestValidateDoesNotMutate(t *testing.T) {
	c := Default()
	c.Executor = "" // make it invalid
	before := c
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for empty executor")
	}
	if c != before {
		t.Fatalf("Validate mutated config: before=%+v after=%+v", before, c)
	}
}

// marshal round-trips through decodeStrict, exercising the on-disk form.
func TestMarshalRoundTrip(t *testing.T) {
	want := Default()
	raw, err := want.marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := decodeStrict(raw)
	if err != nil {
		t.Fatalf("decodeStrict: %v", err)
	}
	if got != want {
		t.Fatalf("round-trip mismatch: got=%+v want=%+v", got, want)
	}

	// Confirm json tags are the stable on-disk names.
	var asMap map[string]json.RawMessage
	if err := json.Unmarshal(raw, &asMap); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}
	for _, key := range []string{"version", "executor", "runtime", "model", "max_steps"} {
		if _, ok := asMap[key]; !ok {
			t.Fatalf("expected json key %q in marshalled config: %s", key, raw)
		}
	}
}

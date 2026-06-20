package onboard

import (
	"encoding/json"
	"strings"
	"testing"

	"nilcore/internal/egressprofile"
)

// TestWebConfigProfile is the P11-T27 acceptance suite: the two additive egress
// fields round-trip, stay omitempty (byte-identical default config), validate
// against the egressprofile.Names() closed set, and arrive via the wizard env map.

// validBase builds a Config that passes Validate, so each subtest mutates only the
// web egress knobs under test.
func validBase() Config {
	return Config{
		Version:   CurrentConfigVersion,
		Runtime:   "podman",
		Backend:   "native",
		Providers: []ProviderConfig{{Name: "anthropic", KeyRef: "k"}},
		Channel:   ChannelConfig{Type: "none"},
	}
}

// TestWebConfigProfileRoundTrip asserts both new fields survive a marshal/unmarshal.
func TestWebConfigProfileRoundTrip(t *testing.T) {
	cfg := validBase()
	cfg.Web = WebConfig{Enabled: true, Profile: "finance", ProfileFile: ".nilcore/egress.json"}

	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Config
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Web.Profile != "finance" {
		t.Errorf("profile not persisted: got %q", got.Web.Profile)
	}
	if got.Web.ProfileFile != ".nilcore/egress.json" {
		t.Errorf("profile_file not persisted: got %q", got.Web.ProfileFile)
	}
}

// TestWebConfigProfileOmitempty is the byte-identical proof: a WebConfig with no
// profile must marshal WITHOUT the profile/profile_file keys, so an existing
// config.json written before P11-T27 is unaffected.
func TestWebConfigProfileOmitempty(t *testing.T) {
	b, err := json.Marshal(WebConfig{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	if strings.Contains(s, "profile") {
		t.Errorf("empty WebConfig leaked a profile key: %s", s)
	}
	// A WebConfig carrying only the pre-existing fields must likewise omit them.
	b2, _ := json.Marshal(WebConfig{Enabled: true, Allow: []string{"docs.io"}, Search: "ddg"})
	if strings.Contains(string(b2), "profile_file") || strings.Contains(string(b2), "\"profile\"") {
		t.Errorf("non-profile WebConfig leaked a profile key: %s", b2)
	}
}

// TestWebConfigProfileValidate covers valid (every Names() entry), empty (allowed),
// and bogus (rejected with an actionable error).
func TestWebConfigProfileValidate(t *testing.T) {
	// Empty profile is always allowed (default).
	cfg := validBase()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("empty profile rejected: %v", err)
	}

	// Every member of the closed set validates.
	for _, name := range egressprofile.Names() {
		c := validBase()
		c.Web.Profile = name
		if err := c.Validate(); err != nil {
			t.Errorf("preset %q rejected by Validate: %v", name, err)
		}
	}

	// A non-member is rejected, and the error names the valid set (actionable).
	bad := validBase()
	bad.Web.Profile = "definitely-not-a-profile"
	err := bad.Validate()
	if err == nil {
		t.Fatalf("bogus profile accepted")
	}
	if !strings.Contains(err.Error(), "web.profile") {
		t.Errorf("error not actionable (no field name): %v", err)
	}
	for _, name := range egressprofile.Names() {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error omits valid value %q: %v", name, err)
		}
	}
}

// TestWebConfigProfileNamesConsistency proves the valid set is sourced from
// egressprofile.Names(): every Names() entry validates and a non-member fails.
func TestWebConfigProfileNamesConsistency(t *testing.T) {
	names := egressprofile.Names()
	if len(names) == 0 {
		t.Fatal("egressprofile.Names() is empty; nothing to validate against")
	}
	for _, n := range names {
		if !validEgressProfile(n) {
			t.Errorf("validEgressProfile rejected a Names() member %q", n)
		}
	}
	if validEgressProfile("__not_a_member__") {
		t.Error("validEgressProfile accepted a non-member")
	}
}

// TestWebConfigProfileFromEnv proves wizard.FromEnv maps NILCORE_EGRESS_PROFILE
// into Web.Profile, and that unset leaves it empty (and the config still valid).
func TestWebConfigProfileFromEnv(t *testing.T) {
	preset := egressprofile.Names()[0]

	// Set: profile flows through. Web egress is also enabled here so Validate sees
	// a representative config; the profile mapping itself is unconditional.
	env := map[string]string{
		"ANTHROPIC_API_KEY":      "sk-x",
		"NILCORE_WEB_ALLOW":      "docs.io",
		"NILCORE_EGRESS_PROFILE": preset,
	}
	store := newMapStore()
	cfg, err := FromEnv(func(k string) string { return env[k] }, store)
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.Web.Profile != preset {
		t.Errorf("profile = %q, want %q", cfg.Web.Profile, preset)
	}

	// Unset: profile stays empty.
	env2 := map[string]string{"ANTHROPIC_API_KEY": "sk-x"}
	cfg2, err := FromEnv(func(k string) string { return env2[k] }, newMapStore())
	if err != nil {
		t.Fatalf("FromEnv (unset): %v", err)
	}
	if cfg2.Web.Profile != "" {
		t.Errorf("unset profile = %q, want empty", cfg2.Web.Profile)
	}
}

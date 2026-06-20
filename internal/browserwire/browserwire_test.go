package browserwire

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// TestShellSingleQuote asserts the escaper round-trips arbitrary bytes through a
// real `sh -c` as EXACTLY ONE argument — the security claim that model-supplied
// flow data can never break out of the quoting to smuggle a second command.
func TestShellSingleQuote(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"plain", "hello"},
		{"empty", ""},
		{"single-quote", "it's"},
		{"many-quotes", "'''"},
		{"backslash", `a\b\\c`},
		{"command-subst", "$(rm -rf /)"},
		{"backtick", "`whoami`"},
		{"semicolon-and", "a; b && c | d"},
		{"newline", "line1\nline2"},
		{"dollar-var", "$HOME ${PATH}"},
		{"json-payload", `[{"action":"type","selector":"#q","text":"a'b\"c"}]`},
		{"mixed-evil", "'; touch /tmp/pwned #\n$(id)`uname`"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			quoted := ShellSingleQuote(tc.in)

			// The whole point: `printf %s <quoted>` must emit the original bytes
			// verbatim, proving the shell sees exactly one inert argument.
			out, err := exec.Command("sh", "-c", "printf %s "+quoted).Output()
			if err != nil {
				t.Fatalf("sh -c failed for %q (quoted=%q): %v", tc.in, quoted, err)
			}
			if string(out) != tc.in {
				t.Fatalf("round-trip mismatch:\n in:   %q\n quoted:%q\n out:  %q", tc.in, quoted, string(out))
			}

			// And it really is a single argument: with `set -- <quoted>`, $# must be 1.
			cnt, err := exec.Command("sh", "-c", "set -- "+quoted+"; printf %s $#").Output()
			if err != nil {
				t.Fatalf("argc check failed for %q: %v", tc.in, err)
			}
			if string(cnt) != "1" {
				t.Fatalf("expected exactly 1 argument for %q, got argc=%q (quoted=%q)", tc.in, string(cnt), quoted)
			}
		})
	}
}

// FuzzShellSingleQuote drives the same single-argument round-trip on random
// bytes. NUL is not representable in a shell argument, so it is skipped.
func FuzzShellSingleQuote(f *testing.F) {
	for _, seed := range []string{"", "'", `\`, "$(x)", "a\nb", "`y`", `[{"a":"'"}]`} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if strings.ContainsRune(s, 0) {
			t.Skip("NUL cannot ride in a shell argument")
		}
		quoted := ShellSingleQuote(s)
		out, err := exec.Command("sh", "-c", "printf %s "+quoted).Output()
		if err != nil {
			t.Fatalf("sh -c failed for %q: %v", s, err)
		}
		if string(out) != s {
			t.Fatalf("round-trip mismatch: in=%q out=%q quoted=%q", s, string(out), quoted)
		}
	})
}

// TestObservationJSON pins the Observation JSON contract: the tags the driver
// emits decode into the typed fields, and unknown fields are ignored (data, not
// instructions — I7).
func TestObservationJSON(t *testing.T) {
	raw := `{"title":"Home","text":"hello world","console":["a","b"],` +
		`"screenshot_b64":"QUJD","unknown_future_field":42}`
	var obs Observation
	if err := json.Unmarshal([]byte(raw), &obs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if obs.Title != "Home" || obs.Text != "hello world" {
		t.Fatalf("title/text not decoded: %+v", obs)
	}
	if len(obs.Console) != 2 || obs.Console[0] != "a" || obs.Console[1] != "b" {
		t.Fatalf("console not decoded: %+v", obs.Console)
	}
	if obs.ScreenshotB64 != "QUJD" {
		t.Fatalf("screenshot not decoded: %q", obs.ScreenshotB64)
	}

	// Re-marshal keeps the exact tag spelling the driver contract requires.
	out, err := json.Marshal(obs)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"title"`, `"text"`, `"console"`, `"screenshot_b64"`} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("marshalled JSON missing tag %s: %s", want, out)
		}
	}
}

package eventlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestChainIntegrity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ev.jsonl")
	log, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		log.Append(Event{Task: "t1", Kind: "step", Detail: map[string]any{"i": i}})
	}
	log.Close()

	if err := Verify(path); err != nil {
		t.Fatalf("Verify a good chain: %v", err)
	}

	// Tamper with the middle event's payload — the chain must catch it.
	data, _ := os.ReadFile(path)
	tampered := strings.Replace(string(data), `"i":1`, `"i":99`, 1)
	if tampered == string(data) {
		t.Fatal("test setup: nothing replaced")
	}
	if err := os.WriteFile(path, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Verify(path); err == nil {
		t.Fatal("Verify should detect the tampered event")
	}
}

func TestChainContinuesAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ev.jsonl")
	l1, _ := Open(path)
	l1.Append(Event{Task: "t", Kind: "a"})
	l1.Close()

	l2, _ := Open(path) // must continue the chain, not restart it
	l2.Append(Event{Task: "t", Kind: "b"})
	l2.Close()

	if err := Verify(path); err != nil {
		t.Fatalf("chain across reopen: %v", err)
	}
}

func TestRedaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ev.jsonl")
	log, _ := Open(path)
	log.Append(Event{Task: "t", Kind: "tool_exec", Detail: map[string]any{
		"cmd":     "export ANTHROPIC_API_KEY=sk-abc123def456ghi789jkl",
		"api_key": "sk-shouldbegone",
		"note":    "harmless",
	}})
	log.Close()

	b, _ := os.ReadFile(path)
	s := string(b)
	if strings.Contains(s, "sk-abc123def456ghi789jkl") {
		t.Error("embedded key not redacted from cmd")
	}
	if strings.Contains(s, "sk-shouldbegone") {
		t.Error("secret-named field not redacted")
	}
	if !strings.Contains(s, "[redacted]") {
		t.Error("expected a redaction marker")
	}
	if !strings.Contains(s, "harmless") {
		t.Error("non-secret content should be preserved")
	}
	// Redaction happens before hashing, so the chain still verifies.
	if err := Verify(path); err != nil {
		t.Fatalf("Verify after redaction: %v", err)
	}
}

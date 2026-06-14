package secrets

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// ExternalStore delegates to a user-configured command — the "external hook" for
// corporate secret managers (Vault, cloud KMS wrappers, etc.). The command is
// invoked as `Command Args... <op> <name>` where op is get|set|delete; for set
// the value is passed on stdin, and get returns the value on stdout. The value
// never appears in argv.
type ExternalStore struct {
	Command string
	Args    []string
}

// Name identifies the backend.
func (e ExternalStore) Name() string { return "external" }

// Get fetches the secret via the hook (value read from stdout).
func (e ExternalStore) Get(name string) (string, error) {
	out, err := e.run(nil, "get", name)
	if err != nil {
		return "", fmt.Errorf("secret %q: %w", name, ErrNotFound)
	}
	return strings.TrimRight(out, "\n"), nil
}

// Set writes the secret via the hook (value passed on stdin).
func (e ExternalStore) Set(name, value string) error {
	if _, err := e.run(strings.NewReader(value), "set", name); err != nil {
		return fmt.Errorf("external set %q: %w", name, err)
	}
	return nil
}

// Delete removes the secret via the hook.
func (e ExternalStore) Delete(name string) error {
	if _, err := e.run(nil, "delete", name); err != nil {
		return fmt.Errorf("external delete %q: %w", name, err)
	}
	return nil
}

func (e ExternalStore) run(stdin *strings.Reader, op, name string) (string, error) {
	if e.Command == "" {
		return "", fmt.Errorf("external store: no command configured")
	}
	args := append(append([]string{}, e.Args...), op, name)
	cmd := exec.Command(e.Command, args...)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return out.String(), nil
}

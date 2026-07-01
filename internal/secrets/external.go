package secrets

import (
	"bytes"
	"errors"
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

// Get fetches the secret via the hook (value read from stdout). It distinguishes a
// hook that RAN and reported the secret absent (a non-zero exit ⇒ ErrNotFound, so the
// resolver falls through to the next store) from a hook that could not be RUN at all
// (missing command, permission denied ⇒ a misconfiguration error, surfaced loudly so
// the operator notices instead of silently treating every secret as absent).
//
// A zero-exit hook that emits nothing (empty / whitespace-only stdout) is also treated
// as ErrNotFound, never as an empty secret: fail closed (I3). A misconfigured broker
// that silently returns nothing must fall through to the next store rather than inject
// an empty credential — matching EnvStore, which likewise rejects an empty value.
func (e ExternalStore) Get(name string) (string, error) {
	out, err := e.run(nil, "get", name)
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return "", fmt.Errorf("secret %q (external hook exit %d): %w", name, exit.ExitCode(), ErrNotFound)
		}
		return "", fmt.Errorf("external get %q: %w", name, err) // hook could not run
	}
	v := strings.TrimRight(out, "\n")
	if strings.TrimSpace(v) == "" {
		return "", fmt.Errorf("secret %q (external hook returned empty): %w", name, ErrNotFound)
	}
	return v, nil
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

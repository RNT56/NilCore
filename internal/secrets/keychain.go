package secrets

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// KeychainStore keeps secrets in the OS keychain via the platform CLI: macOS
// `security`, Linux `secret-tool` (libsecret). It is the preferred desktop
// backend — the OS guards the secrets at rest.
type KeychainStore struct {
	Service string // keychain service/collection label; defaults to "nilcore"
}

// Name identifies the backend.
func (k KeychainStore) Name() string { return "keychain" }

func (k KeychainStore) service() string {
	if k.Service != "" {
		return k.Service
	}
	return "nilcore"
}

// keychainAvailable reports whether this host has a usable keychain CLI.
func keychainAvailable() bool {
	switch runtime.GOOS {
	case "darwin":
		_, err := exec.LookPath("security")
		return err == nil
	case "linux":
		_, err := exec.LookPath("secret-tool")
		return err == nil
	default:
		return false
	}
}

// Get reads the secret from the keychain.
func (k KeychainStore) Get(name string) (string, error) {
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("security", "find-generic-password",
			"-s", k.service(), "-a", name, "-w").Output()
		if err != nil {
			return "", fmt.Errorf("secret %q: %w", name, ErrNotFound)
		}
		return strings.TrimRight(string(out), "\n"), nil
	case "linux":
		out, err := exec.Command("secret-tool", "lookup",
			"service", k.service(), "account", name).Output()
		if err != nil || len(out) == 0 {
			return "", fmt.Errorf("secret %q: %w", name, ErrNotFound)
		}
		return string(out), nil
	default:
		return "", fmt.Errorf("keychain unsupported on %s", runtime.GOOS)
	}
}

// Set stores (or updates) the secret in the keychain.
func (k KeychainStore) Set(name, value string) error {
	switch runtime.GOOS {
	case "darwin":
		// -U updates if present. (security reads the value from argv; that is the
		// documented path. NilCore never logs it.)
		cmd := exec.Command("security", "add-generic-password",
			"-U", "-s", k.service(), "-a", name, "-w", value)
		return runQuiet(cmd, "keychain set")
	case "linux":
		// secret-tool reads the value from stdin — never argv.
		cmd := exec.Command("secret-tool", "store",
			"--label", "nilcore:"+name, "service", k.service(), "account", name)
		cmd.Stdin = strings.NewReader(value)
		return runQuiet(cmd, "keychain set")
	default:
		return fmt.Errorf("keychain unsupported on %s", runtime.GOOS)
	}
}

// Delete removes the secret from the keychain.
func (k KeychainStore) Delete(name string) error {
	switch runtime.GOOS {
	case "darwin":
		cmd := exec.Command("security", "delete-generic-password", "-s", k.service(), "-a", name)
		return runQuiet(cmd, "keychain delete")
	case "linux":
		cmd := exec.Command("secret-tool", "clear", "service", k.service(), "account", name)
		return runQuiet(cmd, "keychain delete")
	default:
		return fmt.Errorf("keychain unsupported on %s", runtime.GOOS)
	}
}

// runQuiet runs cmd, wrapping any failure (without echoing stdin/argv values).
func runQuiet(cmd *exec.Cmd, what string) error {
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}

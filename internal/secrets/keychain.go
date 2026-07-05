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

	// run is the exec seam, injected only in tests so the get/set/delete
	// round-trip is exercisable without touching the real OS keychain. nil (the
	// default) uses the real platform CLI, so production behaviour is unchanged.
	// It receives the CLI name, its args, and optional stdin, and returns stdout.
	run func(name string, args []string, stdin string) (string, error)
}

// Name identifies the backend.
func (k KeychainStore) Name() string { return "keychain" }

func (k KeychainStore) service() string {
	if k.Service != "" {
		return k.Service
	}
	return "nilcore"
}

// exec runs the CLI through the injected seam (tests) or the real os/exec path.
// stdin, when non-empty, is fed on the command's standard input (the secret-tool
// store path passes the value this way so it never reaches argv).
func (k KeychainStore) exec(name string, args []string, stdin string) (string, error) {
	if k.run != nil {
		return k.run(name, args, stdin)
	}
	cmd := exec.Command(name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.Output()
	return string(out), err
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
		out, err := k.exec("security",
			[]string{"find-generic-password", "-s", k.service(), "-a", name, "-w"}, "")
		if err != nil {
			return "", fmt.Errorf("secret %q: %w", name, ErrNotFound)
		}
		v := strings.TrimRight(out, "\n")
		// Fail closed (I3): a zero-exit CLI that returns an empty / whitespace-only
		// value is treated as absent, never as an empty secret, so the resolver
		// chain (keychain is FIRST) falls through instead of injecting "". Mirrors
		// ExternalStore.Get / EnvStore.Get.
		if strings.TrimSpace(v) == "" {
			return "", fmt.Errorf("secret %q (keychain returned empty): %w", name, ErrNotFound)
		}
		return v, nil
	case "linux":
		out, err := k.exec("secret-tool",
			[]string{"lookup", "service", k.service(), "account", name}, "")
		if err != nil {
			return "", fmt.Errorf("secret %q: %w", name, ErrNotFound)
		}
		// secret-tool emits no trailing newline, but trim to match the macOS path
		// so the value round-trips identically across platforms.
		v := strings.TrimRight(out, "\n")
		// Same fail-closed rule as darwin: a whitespace-only payload (which the old
		// len(out)==0 guard missed) resolves as ErrNotFound.
		if strings.TrimSpace(v) == "" {
			return "", fmt.Errorf("secret %q (keychain returned empty): %w", name, ErrNotFound)
		}
		return v, nil
	default:
		return "", fmt.Errorf("keychain unsupported on %s", runtime.GOOS)
	}
}

// Set stores (or updates) the secret in the keychain.
func (k KeychainStore) Set(name, value string) error {
	switch runtime.GOOS {
	case "darwin":
		// SECURITY (I3): keep the plaintext secret OFF argv. A trailing `-w` with NO
		// value tells `security add-generic-password` to read the password from its
		// prompt — which, when stdin is not a tty, is standard input (man security:
		// "Put at end of command to be prompted (recommended)"). We feed the value on
		// stdin via the exec seam, so it never lands on the process command line where
		// any same-user `ps` could read it. This mirrors the Linux secret-tool path.
		// -U updates in place if the item already exists. NilCore never logs the value.
		_, err := k.exec("security",
			[]string{"add-generic-password", "-U", "-s", k.service(), "-a", name, "-w"}, value)
		return wrapQuiet(err, "keychain set")
	case "linux":
		// secret-tool reads the value from stdin — never argv.
		_, err := k.exec("secret-tool",
			[]string{"store", "--label", "nilcore:" + name, "service", k.service(), "account", name}, value)
		return wrapQuiet(err, "keychain set")
	default:
		return fmt.Errorf("keychain unsupported on %s", runtime.GOOS)
	}
}

// Delete removes the secret from the keychain.
func (k KeychainStore) Delete(name string) error {
	switch runtime.GOOS {
	case "darwin":
		_, err := k.exec("security",
			[]string{"delete-generic-password", "-s", k.service(), "-a", name}, "")
		return wrapQuiet(err, "keychain delete")
	case "linux":
		_, err := k.exec("secret-tool",
			[]string{"clear", "service", k.service(), "account", name}, "")
		return wrapQuiet(err, "keychain delete")
	default:
		return fmt.Errorf("keychain unsupported on %s", runtime.GOOS)
	}
}

// wrapQuiet wraps a CLI failure (without echoing stdin/argv values).
func wrapQuiet(err error, what string) error {
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}

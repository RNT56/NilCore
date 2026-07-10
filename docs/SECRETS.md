# Credentials & secrets

How NilCore stores provider and tool credentials, and the one guarantee that matters: **the model never sees a secret in plaintext.** This document is the operational detail behind invariant **I3** (no ambient authority) in `docs/ARCHITECTURE.md`.

## 1. The principle

Secrets are held by the **host process** (NilCore), never by the model. The model emits text and tool calls; *NilCore* attaches credentials when it makes a call or spawns a process. A key the model never sees is a key it cannot leak, be tricked into printing, or exfiltrate via injection.

## 2. What is stored

| Secret | Used by |
|---|---|
| `ANTHROPIC_API_KEY` | Anthropic provider, Claude Code backend |
| `OPENAI_API_KEY` | OpenAI provider |
| `OPENROUTER_API_KEY` | OpenRouter provider |
| `CODEX_API_KEY` | Codex backend (injected per invocation) |
| `TELEGRAM_BOT_TOKEN` / `SLACK_*` | the chat channel |
| per-server MCP credentials | MCP code-execution environment |
| `NILCORE_EMBED_KEY` | semantic code search embedder (opt-in; off ⇒ lexical fallback) |
| `NILCORE_FORGE_TOKEN` | gated draft-PR open (`watch`/`schedule --open-pr`); the agent never merges |
| `NILCORE_WEBHOOK_SECRET` | `serve --webhook` HMAC verification of SCM/CI signatures |

## 3. SecretStore backends

A `SecretStore` interface with pluggable backends. On the read path the resolver tries them **in this order** — the first backend that holds the named secret wins, and the environment is always the final fallback:

1. **OS keychain** (zero-config auto-detected where its CLI is present): **macOS Keychain** (`security`); **Linux Secret Service** (`secret-tool`: libsecret / gnome-keyring / KWallet) over D-Bus.
2. **Encrypted-file vault** (headless VPS; consulted only when the vault already exists on disk): an AES-256-GCM file (`secrets.vault`) at the config path, sealed by a 32-byte master key held in a `0600` key-file (`secrets.key`) or derived from a passphrase (§8). This is the path that works with no desktop session — `nilcore init` provisions it automatically when no keychain is present, and the run path reads it back.
3. **External** command hook (production option; active only when configured): set **`NILCORE_SECRET_EXTERNAL_CMD`** to a command line (e.g. a Vault / cloud-KMS wrapper). NilCore invokes it as `<cmd> get <name>` and reads the value from stdout — the value never lands on argv. A hook that cannot be run is a loud misconfiguration error; a hook that runs and reports the secret absent (or emits nothing) falls through to the next backend. Consulted **before** the ambient environment fallback.
4. **Environment** (CI / advanced; always tried last): the read-only `EnvStore`, read directly from the process environment.

Only the keychain and the environment are auto-detected with no setup; the encrypted vault and the external hook are used only when explicitly provisioned (a key/vault on disk, or `NILCORE_SECRET_EXTERNAL_CMD`). When no keychain CLI is available, `nilcore init` falls back to the encrypted-file vault (key-file default) rather than the read-only environment store, so onboarding succeeds on a headless host; the run path opens the same vault only if it exists, so a pure-environment run never writes files. Writes (`nilcore secret set`) target the keychain or the file vault — never the read-only environment store or the external hook.

> **Keychain writes keep the secret off argv.** Storing a secret on macOS runs `security add-generic-password -U … -w` with the value fed on **standard input** (a trailing `-w` with no inline value tells `security` to read the password from stdin), so the plaintext never appears in the process's command line where a same-user `ps` could read it. Linux (`secret-tool store`) likewise takes the value on stdin. Reads (`find-generic-password -w` on macOS, `secret-tool lookup` on Linux) never put the secret in argv either, and NilCore never logs the value. *(Earlier builds passed the secret as a `security` command-line argument, briefly exposing it via `ps`; that argv exposure has been removed — the value is now stdin-only on both platforms.)*

## 4. The trust boundary

```
SecretStore  ──▶  NilCore host process (in memory, transient)
                       │
        ┌──────────────┴───────────────┐
        ▼                              ▼
  HTTP request headers           child-process env at spawn
  (provider calls NilCore        (Codex, Claude Code, the sandbox,
   makes itself)                  MCP code execution)
        │                              │
        └──────────────┬───────────────┘
                       ▼
              never a prompt, a message, or model context
```

The model sits **outside** this boundary. It never receives a key, and it never needs one — NilCore performs the privileged call on its behalf.

## 5. Injection (per run)

Credentials reach subprocesses through the **environment, at spawn time, for that run only** (this is task P2-T03):

- Provider calls from the native loop: NilCore sets the auth header itself (Bearer for OpenAI/OpenRouter, `x-api-key` for Anthropic). The key is in host memory and the request, nowhere else.
- Delegated CLIs: `CODEX_API_KEY` is set inline for the single `codex exec` run; Claude Code uses `claude login` or `ANTHROPIC_API_KEY` injected into its env.
- Sandbox & MCP: the container and the MCP code-execution environment receive only the env they need for the task; nothing is written to the image, the worktree, or disk.
- Opt-in capability credentials (`NILCORE_EMBED_KEY`, `NILCORE_FORGE_TOKEN`, `NILCORE_WEBHOOK_SECRET`): resolved through the same SecretStore, held transiently in host memory, and injected per request/run — the embedder key on the embeddings request header, the forge token into the approved prepare step (push only, never a merge), the webhook secret used host-side to verify inbound HMAC signatures. None is written to disk in plaintext, logged, or given to the model (I3). The SecretStore model itself is unchanged.

## 6. Redaction

Defense in depth for the path back *into* context:

- **Output redaction:** sandbox/tool/command output is scrubbed for secret patterns (and the known stored values) before it is added to the model's context — so an accidental `env` dump can't leak a key to the model.
- **Log redaction:** the append-only event log redacts secrets before write (task P2-T06).

## 7. Config holds references, not secrets

The config file (JSON, at the platform config path) records *which* providers and channels are enabled and *which* secret name each uses. The secret values live only in the SecretStore. The config is safe to read, diff, and (if you wish) commit; it contains no credentials.

At run time NilCore resolves each credential **environment-first, then the SecretStore** via the config's reference: an exported variable (e.g. `OPENROUTER_API_KEY`) always wins, otherwise the key captured by `nilcore init` is fetched from the SecretStore by name. So a configured host runs with no re-exporting, while an env var still overrides for one-off use. With no config and no keychain, resolution is pure environment lookup. The model executor, container runtime, and sandbox image fall back to `config.json` the same way (an explicit flag or env var overrides the configured value).

## 8. Headless-VPS master key (the one trade-off)

On a bare Linux VPS there is no keychain, so the encrypted vault needs a master key available at boot. Options, least to most operationally convenient:

- **Startup passphrase** — `nilcore init -vault passphrase`. Derives the master key from a passphrase + a per-vault random salt (PBKDF2-HMAC-SHA256, 200k iterations; the salt is stored at `secrets.salt`, the key never is). Most secure; for unattended run/serve the passphrase is supplied via `NILCORE_VAULT_PASSPHRASE` (e.g. a systemd `EnvironmentFile`), otherwise `init` prompts for it. No master-key file sits on disk.
- **systemd-creds / cloud KMS** — platform-managed; unattended and strong. Recommended for production (constructed directly / external store).
- **Key-file (`0600`, owner-only)** — `nilcore init` default. Unattended and simple; only as strong as the host's filesystem and access controls.

`nilcore init` selects the backend automatically: the OS keychain when its CLI is present, otherwise the encrypted-file vault. The vault's master-key strategy defaults to the **key-file**; pass `-vault passphrase` to seal it with a passphrase instead. Switching modes on an existing vault is refused (it would leave prior entries undecryptable) — remove `secrets.key`/`secrets.salt` + `secrets.vault` to start over. `nilcore doctor` warns when a passphrase vault is in use but `NILCORE_VAULT_PASSPHRASE` is unset.

## 9. Provider & CLI auth reference

- **OpenAI / OpenRouter:** `Authorization: Bearer <key>`. OpenRouter's base URL is `https://openrouter.ai/api/v1`; models are namespaced `provider/model`.
- **Anthropic:** `x-api-key: <key>` + `anthropic-version`.
- **Codex:** `CODEX_API_KEY` is honored only by `codex exec`, set inline per run; treat `~/.codex/auth.json` as a password.
- **Claude Code:** `claude login`, or `ANTHROPIC_API_KEY` in its env.

All of the above are obtained and stored via `nilcore init` (see `docs/PREREQUISITES.md`); none are entered or echoed where the model can see them.

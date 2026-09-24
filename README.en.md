# agsw

**English** | [中文文档](README.md)

[![CI](https://github.com/kylesean/agsw/actions/workflows/ci.yml/badge.svg)](https://github.com/kylesean/agsw/actions/workflows/ci.yml)
[![Go Version](https://img.shields.io/github/go-mod/go-version/kylesean/agsw)](https://go.dev/)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![GitHub Release](https://img.shields.io/github/v/release/kylesean/agsw)](https://github.com/kylesean/agsw/releases)

Multi-account switching and reverse proxy manager for Google Antigravity CLI (`agy`).

## Background

By default, `agy` stores its OAuth credentials in the system Secret Service (GNOME Keyring / macOS Keychain / Windows Credential Manager) and only maintains a single active credential entry at any given time. When your personal Google account hits rate limits (5-hour generation quota or weekly quota), you have to manually re-authenticate via the browser, which overwrites the existing credentials.

`agsw` provides an isolated local credential pool, a bidirectional protocol-rewriting reverse proxy, and proactive quota-aware rotation to enable smooth, automatic multi-account switching without manual re-login interruptions.

## Architecture & Protocol Conversion

The communication protocols differ between `agy`'s gateway mode and native mode. `agsw serve` acts as an intelligent reverse proxy handling bidirectional protocol conversion:

- **Client Interface**: `agy` gateway mode uses standard Gemini REST:
  `POST /v1beta/models/{model}:streamGenerateContent?alt=sse`
- **Upstream Backend**: Antigravity CloudCode backend (`daily-cloudcode-pa.googleapis.com`):
  `POST /v1internal:streamGenerateContent?alt=sse`
- **Envelope Transformation**:
  ```
  agy (client)   POST /v1beta/models/{model}:streamGenerateContent?alt=sse
                 body = standard Gemini GenerateContentRequest
  agsw (proxy)   POST /v1internal:streamGenerateContent
                 body = {"model":"<upstream_real_key>","request":<raw_body_bytes>}
  Upstream API   data: {"response":{candidates...},"traceId":...,"metadata":{}}
  agsw (proxy)   data: {candidates...} (unwrapped back to standard REST format)
  ```

### Key Protocol & Transport Features

1. **User-Agent Authentication Gate**:
   Upstream strictly validates client identity. The proxy automatically injects the required gate header: `User-Agent: antigravity-cli/1.2.9`.
2. **Dynamic Model Alias Resolution**:
   Client model names (e.g. `gemini-3.8-flash`) may require specific upstream keys (e.g. `gemini-3.8-flash-tiered`). On startup, `agsw` queries `fetchAvailableModels` to resolve aliases, with resilient background retry if initial network glitches occur.
3. **SSE Delimiter Normalization**:
   Upstream streaming uses CRLF delimiters. The streaming parser normalizes them to LF, preventing client SSE event parsing stalls.
4. **Outbound Proxy Support**:
   The proxy automatically honors system proxy environment variables (`HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY`).

## Installation

### Option 1: Via Go Toolchain (Recommended)

```sh
go install github.com/kylesean/agsw/cmd/agsw@latest
```

### Option 2: Prebuilt Binaries

Download precompiled archives for Linux, macOS, and Windows from [GitHub Releases](https://github.com/kylesean/agsw/releases). Extract the binary and place it in your `PATH`.

## Command Overview

```sh
agsw probe    # Launch traffic inspection probe (diagnose wire protocols)
agsw login    # Independent Google OAuth PKCE login (Recommended)
agsw add      # Import current system Keyring credentials into pool
agsw list     # List all accounts in the pool
agsw status   # View system Keyring status alongside pool summary
agsw drop     # Remove an account from the pool
agsw usage    # Query real quota status of accounts in pool concurrently
agsw          # Recommended: Launch Gateway and managed agy directly
agsw gui      # Compatibility alias: Equivalent to agsw
agsw serve    # Start reverse proxy gateway only
```

### Account Login (`login`)

`agsw login <name>` independently completes the OAuth 2.0 Authorization Code + PKCE flow without touching the system Keyring:

```sh
agsw login A                          # Launch browser to log in and save to pool
agsw login -email you@gmail.com B     # Pre-fill email address
agsw login -no-browser C              # Print authorization URL only (headless / SSH)
agsw login -timeout 10m D             # Custom timeout (default: 5m)
```

> Note: Flags must precede the `<name>` argument.

Workflow:
1. Listens on a local loopback port (`127.0.0.1:0`) at `/auth/callback`.
2. Generates cryptographically secure `state` (anti-CSRF) and PKCE `code_verifier` (S256 challenge).
3. Launches the system default browser with the authorization URL (and prints fallback in terminal).
4. Receives callback `code`, exchanges for full tokens (including `refresh_token`), validates `id_token` claims, and atomically saves credentials with `0600` permissions.

### Unified Launcher (`gui` / `agsw`)

`agsw` (or `agsw gui`) is the **recommended primary entry point**: it launches the local Gateway, waits for the port to be ready, configures `AGY_GATEWAY_URL` and `NO_PROXY`, and spawns `agy`. When `agy` exits, the Gateway shuts down cleanly.
Gateway, quota polling, and rotation logs are written to `~/.cache/agsw/gui.log` without interfering with `agy`'s TUI. You can monitor them via `tail -f ~/.cache/agsw/gui.log`.

Interactive `agy` sessions enable `-sync-keyring` by default: when the active account changes, `agsw` gracefully waits for in-flight streaming requests to finish before updating the system Keyring and restarting the managed `agy` instance. Simply execute `/resume` in `agy` to resume your conversation with the new account identity.

```sh
# Start managed interactive agy session (recommended, shortest command)
agsw

# Equivalent command using the explicit alias
agsw gui

# Switch early when remaining quota drops below 0.2%
agsw -quota-threshold=0.002 -- --dangerously-skip-permissions

# Pass arbitrary flags to agy after --
agsw -- --print 'hi'

# Disable Keyring synchronization and auto-restart
agsw -sync-keyring=false
```

One-shot commands (`agy --print`) do not trigger auto-restart; they simply route requests through the Gateway. If upstream returns 429 before response streaming starts, the Gateway temporarily cools down the current account and replays the request with the next healthy account.

To inspect actual account pool quotas, run:

```sh
agsw usage
agsw usage -account B
```

### Standalone Proxy (`serve`)

```sh
# Start reverse proxy (default: 127.0.0.1:8085)
agsw serve

# Manual gateway integration with agy
AGY_GATEWAY_URL=http://127.0.0.1:8085 agy --print 'hi'

# Common options
agsw serve -account main      # Pin to a single account
agsw serve -refresh           # Force pre-refresh tokens at startup
agsw serve -v                 # Verbose routing & rotation logs
agsw serve -quota-interval 1m # Quota polling interval (default: 1m)
```

### Quota Detection & Auto-Rotation

1. **Read-Only Quota Monitoring**:
   Periodically queries `v1internal:retrieveUserQuotaSummary` in the background to inspect 5-hour and weekly quota sliding windows without consuming generation quota.
2. **Active Account Affinity**:
   Persistently sticks to the healthy active account, avoiding unnecessary account flapping when older accounts unfreeze.
3. **Instant 429 Backoff & Retry**:
   Intercepts HTTP 429 (Too Many Requests), instantly applies temporary cooldown (1 minute) to the failing account, triggers immediate fast-path quota re-checks, and replays the request with the next healthy account without header pollution.
4. **Fine-Grained Concurrency Control**:
   Uses per-account mutexes and double-checked locking; token network refreshes for one account never block queries or routing for other healthy accounts.
5. **Idle & Cooldown Token Freshness**:
   Automatically ensures tokens are fresh even for cooling or idle accounts, guaranteeing timely quota reset detection.

### Import Existing Credentials (`add`)

If you have already logged in via `agy`, import the active credentials into the pool:

```sh
agsw add <name>
```

## Security & Permission Controls

- **Secure Local Storage**: Enforces strict `0700` directory and `0600` file permissions with atomic file replacement.
- **Input Validation**: Enforces strict account name regex `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$` preventing directory traversal.
- **Credential Masking**: Automatically masks sensitive headers (`Authorization`, `Cookie`) in diagnostic probe logs.
- **Controlled Keyring Access**: `add` and `status` remain read-only; Keyring writes only occur during managed GUI account switches when enabled (`-sync-keyring=true`).

## Disclaimer

Multi-account rotation is designed to manage development and testing credentials compliantly. Please strictly follow Google's Terms of Service and relevant platform policies.

## Development & Testing

```sh
go build ./...
go test -v -race ./...
go run ./cmd/agsw -h
```

## License

This project is licensed under the [MIT License](LICENSE).

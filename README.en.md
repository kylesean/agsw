# agsw

**English** | [中文文档](README.md)

[![CI](https://github.com/kylesean/agsw/actions/workflows/ci.yml/badge.svg)](https://github.com/kylesean/agsw/actions/workflows/ci.yml)
[![Go Version](https://img.shields.io/github/go-mod/go-version/kylesean/agsw)](https://go.dev/)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![GitHub Release](https://img.shields.io/github/v/release/kylesean/agsw)](https://github.com/kylesean/agsw/releases)

**`agsw`** is an intelligent multi-account credential pool and reverse proxy gateway for Antigravity CLI ([`agy`](https://antigravity.google/download)).

It eliminates quota exhaustion interruptions by maintaining an isolated pool of Google accounts, continuously monitoring 5-hour and weekly quota sliding windows, and seamlessly rotating credentials on the fly.

---

## The Problem

`agy` binds OAuth credentials exclusively to the operating system's credential store (e.g. Secret Service / GNOME Keyring / macOS Keychain) with a single global slot:

- Whenever an account reaches Google's quota limit (5-hour generation bucket or weekly limit), `agy` fails immediately.
- Switching accounts requires manually logging in again through the browser, overwriting the previous credentials.
- Multi-account developers cannot pool quotas or achieve uninterrupted coding workflows.

## The Solution

`agsw` introduces an isolated, encrypted-at-rest credential pool combined with a lightweight, zero-latency reverse proxy:

```
[ Developer Terminal: agy --print "hello" ]
                  │
                  ▼ (Local HTTP / SSE Stream)
[ agsw reverse proxy (127.0.0.1:8085) ]
   ├── ① Selector: Picks healthy account with remaining quota
   ├── ② Auto-Refresh: Refreshes access tokens via Google OAuth2 endpoint
   ├── ③ Protocol Bridge: Rewrites standard Gemini REST ⇄ Cloud Code envelope
   └── ④ Quota Watcher: Backs off on 429 and rotates account before exhaustion
                  │
                  ▼ (HTTPS)
[ Google Upstream API (daily-cloudcode-pa.googleapis.com) ]
```

---

## Features

- **⚡ Zero-Config Transparent Proxy**: Forward traffic with `AGY_GATEWAY_URL=http://127.0.0.1:8085 agy ...`. No configuration changes inside `agy`.
- **🔄 Proactive Quota-Aware Rotation**: Polls read-only quota endpoints (`v1internal:retrieveUserQuotaSummary`) to detect 5-hour and weekly exhaustion *without* consuming generation quota. Automatically cools down exhausted accounts until reset.
- **🛡️ Instant 429 Backoff**: Intercepts upstream HTTP 429 rate limit responses to trigger immediate 1-minute temporary cooldown and activates fast-path quota re-checks.
- **🔑 Independent PKCE OAuth2 Login**: Run `agsw login <name>` with S256 challenge, CSRF protection, and local loopback callback server. Add accounts without touching system keyring or overriding other credentials.
- **🚀 Fine-Grained Non-Blocking Concurrency**: Double-checked locking with per-account mutexes ensures token refresh network I/O never blocks queries for other healthy accounts.
- **🔒 Secure Local Storage**: Enforces strict `0700` directory and `0600` file permissions with atomic file replacement. Redacts sensitive tokens from diagnostic probe logs.

---

## Installation

### Option 1: Via Go Install (Recommended)

```sh
go install github.com/kylesean/agsw/cmd/agsw@latest
```

### Option 2: Prebuilt Binaries

Download precompiled archives for Linux (`amd64`, `arm64`), macOS (Intel & Apple Silicon), and Windows from [GitHub Releases](https://github.com/kylesean/agsw/releases).

Extract the archive and place `agsw` into your `PATH`.

---

## Quick Start

### 1. Add Accounts to the Pool

You can add accounts using the built-in PKCE login flow:

```sh
agsw login account-a
agsw login account-b
```

Or capture your currently active `agy` system credential:

```sh
agsw add current-account
```

Check pool status at any time:

```sh
agsw list
```

### 2. Start the Proxy Gateway

```sh
agsw serve -v
```

By default, the proxy listens on `127.0.0.1:8085`.

### 3. Connect `agy` to `agsw`

Simply prepend `AGY_GATEWAY_URL`:

```sh
AGY_GATEWAY_URL=http://127.0.0.1:8085 agy --print "Write a quicksort in Go"
```

To persist the gateway across shell sessions:

```sh
# Add to your ~/.bashrc or ~/.zshrc
export AGY_GATEWAY_URL=http://127.0.0.1:8085
```

---

## CLI Reference

| Command | Description |
|---|---|
| `agsw login <name>` | Perform independent Google OAuth2 PKCE login and store credentials in the pool |
| `agsw serve` | Start reverse proxy with quota monitoring, token refresh, and auto-rotation |
| `agsw list` | List accounts in pool with token expiry, refresh ability, and quota cooldowns |
| `agsw status` | Display system Keyring status alongside account pool summary |
| `agsw add <name>` | Import existing credentials from system Keyring into the pool |
| `agsw drop <name>` | Safely remove an account from the pool |
| `agsw probe` | Traffic inspection probe for diagnostics (with credential redaction) |

### Key Flags for `agsw serve`

```sh
agsw serve -listen 127.0.0.1:8085   # Set listen address (default: 127.0.0.1:8085)
agsw serve -account <name>          # Pin traffic to a specific account
agsw serve -v                       # Enable verbose request routing & rotation logs
agsw serve -quota-interval 1m       # Background quota polling interval (default: 1m)
agsw serve -quota-threshold 0.0     # Minimum quota fraction before triggering rotation
agsw serve -refresh                 # Force refresh all account tokens upon startup
```

---

## Architecture & Protocol Details

Google Cloud Code gateway mode utilizes a wrapper envelope protocol:

- **Client Request**: `POST /v1beta/models/{model}:streamGenerateContent?alt=sse`
- **Upstream Gateway**: `POST /v1internal:streamGenerateContent?alt=sse`
- **Envelope Rewriting**:
  - Encapsulates payload: `{"model":"<upstream_model_key>","request":<raw_body>}`
  - Unwraps response stream: `data: {"response":{candidates...}}` ➔ `data: {candidates...}`
  - Normalizes CRLF streaming delimiters to standard LF for uninterrupted SSE parsing.
  - Automatically resolves model aliases (e.g. `gemini-3.8-flash` ➔ `gemini-3.8-flash-tiered`) discovered via `fetchAvailableModels`.

---

## Contributing & Testing

```sh
# Run fast inner-loop verification with race detector (< 2s)
go test -count=1 -race ./...

# Build binary locally
go build -o bin/agsw ./cmd/agsw
```

## License

This project is licensed under the [MIT License](LICENSE).

# cliq-openclaw-bridge

A Go service that bridges [Zoho Cliq](https://www.zoho.com/cliq/) and an [OpenClaw](https://openclaw.ai) AI gateway. Zoho Cliq sends webhook events to the bridge, which forwards them to the AI agent and relays agent replies — including files and real-time tool call notifications — back to the originating Zoho channel.

## How it works

```
Zoho Cliq → POST /webhooks/zoho
  → HMAC middleware (validates X-Zoho-Webhook-Token)
  → Worker pool enqueues job
  → Dispatcher.Dispatch() (runs in worker goroutine)
      ├─ Text message → OpenClaw /v1/responses
      └─ File message → OAuth download → save to workspace → OpenClaw /v1/responses
  → Background goroutine tails OpenClaw session JSONL → posts tool call names to Zoho in real time
  → Agent reply delivered back to Zoho Cliq (text + optional file)
```

Each Zoho channel has its own isolated OpenClaw session key (`hook:zoho-cliq:<channelID>`), so conversations stay separate across channels.

## Prerequisites

- Go 1.22+
- A running [OpenClaw](https://openclaw.ai) gateway reachable from this service
- A Zoho Cliq bot with an incoming webhook configured
- Zoho OAuth credentials (for file downloads from Zoho attachments)

## Quick start

```bash
# 1. Clone and build
git clone https://github.com/ahmadpiran/cliq-openclaw-bridge
cd cliq-openclaw-bridge
go build ./...

# 2. Configure
cp .env.example .env
# Edit .env — see Configuration section below

# 3. Run
go run ./cmd/server
```

## Docker

The service ships a `docker-compose.yml` designed to run alongside an OpenClaw gateway on the same Docker network (`cliq-bridge-net`).

```bash
# Copy and fill in the env file the compose file references
cp .env.example .bridge.env
# Edit .bridge.env

docker compose up --build
```

Key compose variables (set in the shell or a `.env` file next to `docker-compose.yml`):

| Variable | Default | Purpose |
|---|---|---|
| `BRIDGE_IMAGE` | `cliq-openclaw-bridge:local` | Image name/tag to use |
| `BRIDGE_ENV_FILE` | `.bridge.env` | Path to the bridge env file |
| `BRIDGE_BUILD_CONTEXT` | `./cliq-openclaw-bridge` | Docker build context |
| `OPENCLAW_CONFIG_DIR` | *(required)* | Host path to OpenClaw config/agents dir — mounted read-only |
| `OPENCLAW_WORKSPACE_DIR` | *(required)* | Host path to OpenClaw workspace — mounted read-write |

## Configuration

All configuration is via environment variables. Copy `.env.example` to `.env` and populate the values.

### Required

| Variable | Description |
|---|---|
| `BRIDGE_NOTIFY_SECRET` | Shared secret for `POST /notify` from OpenClaw hook |
| `ZOHO_WEBHOOK_SECRET` | HMAC secret Zoho signs webhook payloads with |
| `ZOHO_CLIENT_ID` | Zoho OAuth app client ID |
| `ZOHO_CLIENT_SECRET` | Zoho OAuth app client secret |
| `OPENCLAW_BASE_URL` | OpenClaw gateway base URL (e.g. `http://openclaw-gateway:18789`) |
| `OPENCLAW_API_KEY` | Gateway auth token (from `gateway.auth.token` in openclaw.json) |

### Optional — enable full functionality

| Variable | Default | Description |
|---|---|---|
| `ZOHO_REPLY_WEBHOOK_URL` | — | Bot webhook URL for posting replies. Replies are silently dropped if unset. |
| `ZOHO_CLIQ_API_URL` | `https://cliq.zoho.com` | Zoho Cliq API base for file uploads. Adjust for your data centre (`.eu`, `.in`, `.com.au`). |
| `ZOHO_REDIRECT_URI` | — | OAuth callback URL (e.g. `https://your-domain.com/oauth/callback`) |
| `OPENCLAW_AGENTS_DIR` | — | Path to OpenClaw agents dir. Required for real-time tool call notifications. |
| `OPENCLAW_WORKSPACE_DIR` | — | Path to shared workspace. Required for file upload/download. |

### Tuning

| Variable | Default | Description |
|---|---|---|
| `PORT` | `8080` | HTTP listen port |
| `OPENCLAW_REPLY_TIMEOUT` | `120s` | Max wait for an agent reply |
| `OPENCLAW_MAX_RETRIES` | `4` | Retry attempts on gateway errors |
| `OPENCLAW_INITIAL_BACKOFF` | `500ms` | Initial retry backoff |
| `OPENCLAW_MAX_BACKOFF` | `30s` | Maximum retry backoff |
| `WORKER_COUNT` | `8` | Worker goroutines |
| `WORKER_QUEUE_DEPTH` | `512` | Bounded job queue size |
| `WORKER_JOB_TIMEOUT` | `150s` | Per-job timeout |
| `BOLT_DB_PATH` | `/data/tokens.db` | BoltDB path for OAuth token persistence |

## HTTP endpoints

| Method | Path | Description |
|---|---|---|
| `POST` | `/webhooks/zoho` | Receives Zoho Cliq events (HMAC-authenticated) |
| `GET` | `/oauth/callback` | Zoho OAuth authorization callback |
| `POST` | `/notify` | Push endpoint from OpenClaw hook (internal) |
| `GET` | `/healthz` | Health check — returns 200 |

## File handling

**Inbound (Zoho → agent):** The bridge downloads the attachment from Zoho using a valid OAuth token, saves it to `OPENCLAW_WORKSPACE_DIR/uploads/`, and tells the agent the local path.

**Outbound (agent → Zoho):** The agent includes a file tag in its reply:

```
[FILE:~/workspace/path/to/file.pdf]
```

The bridge strips the tag, resolves `~/workspace/` to `OPENCLAW_WORKSPACE_DIR`, and uploads the file to Zoho Cliq using the zapikey from the webhook URL.

## Real-time tool call notifications

When `OPENCLAW_AGENTS_DIR` is set, the bridge tails the OpenClaw session JSONL file while the agent is processing. Each tool call is posted to Zoho Cliq immediately (e.g. `⚙️ read_file`) rather than waiting for the full response.

## OpenClaw hook (optional)

`openclaw/hooks/zoho-bridge-notify/` contains a ready-to-install OpenClaw hook that pushes agent replies to `POST /notify`. This path is dormant — the polling/tailing mechanism above is active by default. The push path activates automatically once OpenClaw implements the `message:sent` event.

To install:

```bash
cp -r openclaw/hooks/zoho-bridge-notify ~/.openclaw/hooks/
openclaw hooks enable zoho-bridge-notify
```

Set these env vars on the OpenClaw gateway container:

| Variable | Description |
|---|---|
| `BRIDGE_NOTIFY_URL` | Bridge notify URL (default: `http://cliq-bridge:8080/notify`) |
| `BRIDGE_NOTIFY_SECRET` | Must match `BRIDGE_NOTIFY_SECRET` in the bridge env |

## Agent instructions

`openclaw/workspace/AGENTS.md` should be placed in the OpenClaw agent's workspace directory. It tells the agent how to use the `[FILE:...]` protocol, the workspace layout, and memory conventions for persisting context across sessions.

## Development

```bash
# Run all tests
go test ./...

# Run tests for a single package
go test ./internal/handler/...

# Run a specific test
go test ./internal/handler/ -run TestDispatcher_forward

# Lint (requires golangci-lint)
golangci-lint run ./...
```

## Project structure

```
cmd/server/          — entry point; wires all components together
internal/
  config/            — env-var config loading; fails fast on missing required vars
  handler/           — HTTP handlers + Dispatcher (core business logic)
  gateway/           — OpenClaw HTTP client with exponential-backoff retry
  zoho/              — Zoho OAuth token refresher and reply sender
  session/           — polls OpenClaw JSONL session files for agent replies
  middleware/        — ZohoHMAC middleware (constant-time token comparison)
  store/             — BoltDB-backed OAuth token persistence
  worker/            — fixed-size goroutine pool with bounded queue
openclaw/
  hooks/zoho-bridge-notify/  — optional OpenClaw push hook
  workspace/AGENTS.md        — agent instructions placed in OpenClaw workspace
```

## Contributing

1. Fork the repo and create a feature branch.
2. Make your changes — ensure `go test ./...` and `golangci-lint run ./...` pass.
3. Open a pull request with a clear description of what changed and why.

Bug reports and feature requests are welcome via GitHub Issues.

## License

[MIT](LICENSE) © 2026 Ahmad Piran

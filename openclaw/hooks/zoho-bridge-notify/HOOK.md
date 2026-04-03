---
name: zoho-bridge-notify
description: "Pushes every OpenClaw agent reply to the Zoho Cliq bridge in real time"
metadata: {"openclaw":{"emoji":"🦞","events":["message","command","session","agent","gateway"]}}
---

# Zoho Bridge Notify

Fires on agent events and forwards assistant messages from the `hook:zoho-cliq`
session to the bridge `POST /notify` endpoint, which delivers them to Zoho Cliq.

This hook is ready for when OpenClaw implements `message:sent` natively.
Currently uses a broad event subscription to catch all events and filters
internally to the zoho-cliq session.

## Environment variables (set in openclaw-gateway container)
- `BRIDGE_NOTIFY_URL`    — default: http://cliq-bridge:8080/notify
- `BRIDGE_NOTIFY_SECRET` — must match BRIDGE_NOTIFY_SECRET in .bridge.env

## Installation
Copy this directory to `~/.openclaw/hooks/zoho-bridge-notify/` on the host
running the OpenClaw gateway, then run:

    openclaw hooks enable zoho-bridge-notify
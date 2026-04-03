.PHONY: build test run docker-build docker-run bootstrap-oauth install-hook install-workspace install

# ── Local development ─────────────────────────────────────────────────────────
build:
	go build -o bin/bridge ./cmd/server

test:
	go test ./...

run: build
	@export $$(cat .env | xargs) && ./bin/bridge

# ── Docker ────────────────────────────────────────────────────────────────────
docker-build:
	docker build -t cliq-openclaw-bridge:latest .

docker-run:
	docker compose up --build

# ── OAuth bootstrap ────────────────────────────────────────────────────────────
# Scopes:
#   ZohoCliq.Files.READ   — download files sent by users in Zoho Cliq
#   ZohoCliq.Files.UPDATE — upload files from the agent back to Zoho Cliq
bootstrap-oauth:
	@export $$(cat .env | xargs) && \
	echo "" && \
	echo "Open this URL in your browser and authorise the app:" && \
	echo "" && \
	echo "  https://accounts.zoho.com/oauth/v2/auth?scope=ZohoCliq.Files.READ,ZohoCliq.Files.UPDATE&client_id=$$ZOHO_CLIENT_ID&response_type=code&redirect_uri=$$ZOHO_REDIRECT_URI&access_type=offline&prompt=consent" && \
	echo "" && \
	echo "After authorising, Zoho redirects to ZOHO_REDIRECT_URI with ?code=<value>." && \
	echo "The bridge /oauth/callback endpoint handles the rest automatically."

# ── OpenClaw setup ─────────────────────────────────────────────────────────────
# OPENCLAW_CONFIG_DIR must be set in your environment or .env file.
# Example: OPENCLAW_CONFIG_DIR=/home/ubuntu/openclaw/config

install-hook:
	@if [ -z "$$OPENCLAW_CONFIG_DIR" ]; then \
		echo "Error: OPENCLAW_CONFIG_DIR is not set."; exit 1; \
	fi
	@mkdir -p "$$OPENCLAW_CONFIG_DIR/hooks/zoho-bridge-notify"
	cp openclaw/hooks/zoho-bridge-notify/HOOK.md \
	   "$$OPENCLAW_CONFIG_DIR/hooks/zoho-bridge-notify/HOOK.md"
	cp openclaw/hooks/zoho-bridge-notify/handler.ts \
	   "$$OPENCLAW_CONFIG_DIR/hooks/zoho-bridge-notify/handler.ts"
	@echo "Hook installed to $$OPENCLAW_CONFIG_DIR/hooks/zoho-bridge-notify/"
	@echo "Run: docker compose exec openclaw-gateway node dist/index.js hooks enable zoho-bridge-notify"

install-workspace:
	@if [ -z "$$OPENCLAW_WORKSPACE_DIR" ]; then \
		echo "Error: OPENCLAW_WORKSPACE_DIR is not set."; exit 1; \
	fi
	@mkdir -p "$$OPENCLAW_WORKSPACE_DIR"
	cp openclaw/workspace/AGENTS.md "$$OPENCLAW_WORKSPACE_DIR/AGENTS.md"
	@echo "AGENTS.md installed to $$OPENCLAW_WORKSPACE_DIR/AGENTS.md"

install: install-hook install-workspace
	@echo ""
	@echo "Installation complete. Next steps:"
	@echo "  1. Edit zoho/message_handler.deluge and paste into your Zoho Cliq bot"
	@echo "  2. Run: make bootstrap-oauth"
	@echo "  3. Restart: docker compose restart openclaw-gateway"
	@echo "  4. Enable hook: docker compose exec openclaw-gateway node dist/index.js hooks enable zoho-bridge-notify"
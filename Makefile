.PHONY: build test run docker-build docker-run bootstrap-oauth

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

# ── OAuth bootstrap (run once before first deployment or after token loss) ─────
# Opens the Zoho consent URL in your browser, then prompts for the code.
bootstrap-oauth:
	@export $$(cat .env | xargs) && \
	echo "" && \
	echo "Open this URL in your browser and authorise the app:" && \
	echo "" && \
	echo "  https://accounts.zoho.com/oauth/v2/auth?scope=ZohoCliq.Channels.UPDATE&client_id=$$ZOHO_CLIENT_ID&response_type=code&redirect_uri=$$ZOHO_REDIRECT_URI&access_type=offline" && \
	echo "" && \
	echo "After authorising, Zoho will redirect to your ZOHO_REDIRECT_URI with ?code=<value>." && \
	echo "The running service handles the exchange at GET /oauth/callback."
# ── Stage 1: Build ────────────────────────────────────────────────────────────
FROM golang:1.25-alpine AS builder

# gcc is required by bbolt's CGO-free build path on Alpine.
RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /src

# Cache dependency downloads as a separate layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-s -w -X main.version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)" \
    -trimpath \
    -o /bin/bridge \
    ./cmd/server

# ── Stage 2: Runtime ──────────────────────────────────────────────────────────
FROM scratch

# ca-certificates: required for TLS handshakes to Zoho and OpenClaw APIs.
# tzdata: required for time zone handling in slog timestamps.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=builder /bin/bridge /bin/bridge

# /data is the mount point for the BoltDB persistent volume.
VOLUME ["/data"]

EXPOSE 8080

ENTRYPOINT ["/bin/bridge"]
package handler

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/ahmadpiran/cliq-openclaw-bridge/internal/worker"
)

// ZohoPayload represents the top-level structure of a Zoho Cliq webhook body.
// We decode only the fields we need for routing and logging at the handler layer.
// The raw bytes are forwarded to the worker intact for full processing.
type ZohoPayload struct {
	Type    string `json:"type"`  // e.g. "message", "slash_command", "bot_action"
	Token   string `json:"token"` // Zoho bot/channel verification token
	Trigger struct {
		Type string `json:"type"` // e.g. "incoming_message", "file_upload"
	} `json:"trigger"`
}

// WebhookHandler holds the dependencies for the Zoho webhook endpoint.
// Constructed once at startup and reused across all requests.
type WebhookHandler struct {
	pool *worker.Pool
}

// NewWebhookHandler creates a WebhookHandler wired to the given worker pool.
func NewWebhookHandler(pool *worker.Pool) *WebhookHandler {
	return &WebhookHandler{pool: pool}
}

// HandleZoho is the HTTP handler for POST /webhooks/zoho.
//
// By the time a request reaches this handler:
//   - The HMAC middleware has already validated the signature.
//   - The body has been restored to r.Body and is safe to read again.
//
// This handler must return as fast as possible — Zoho expects a 200 OK
// within its delivery timeout window. All processing happens in the worker pool.
func (h *WebhookHandler) HandleZoho(w http.ResponseWriter, r *http.Request) {
	requestID := middleware.GetReqID(r.Context())

	// Read the body. The HMAC middleware already capped this at 2 MiB and
	// restored it, so this read is safe and bounded.
	body, err := io.ReadAll(r.Body)
	slog.Debug("raw payload", "body", string(body))
	if err != nil {
		slog.Error("webhook handler: failed to read body",
			"request_id", requestID,
			"error", err,
		)
		http.Error(w, "could not read body", http.StatusInternalServerError)
		return
	}

	// Partial decode for logging and future routing — does not block dispatch.
	var payload ZohoPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		// A non-fatal warning: we still forward malformed payloads to the worker
		// so nothing is silently dropped. The worker can decide what to do.
		slog.Warn("webhook handler: could not decode payload for logging",
			"request_id", requestID,
			"error", err,
		)
	}

	slog.Info("webhook received",
		"request_id", requestID,
		"payload_type", payload.Type,
		"trigger_type", payload.Trigger.Type,
		"body_bytes", len(body),
	)

	job := worker.Job{
		RequestID:  requestID,
		Payload:    body,
		ReceivedAt: time.Now(),
	}

	if err := h.pool.Dispatch(job); err != nil {
		// ErrQueueFull or ErrPoolClosed. We return 429 so Zoho can retry later
		// rather than 500 which Zoho may treat as a permanent failure.
		slog.Warn("webhook handler: dispatch failed",
			"request_id", requestID,
			"error", err,
		)
		http.Error(w, "service busy, retry later", http.StatusTooManyRequests)
		return
	}

	// Respond immediately — Zoho only needs acknowledgement of receipt.
	w.WriteHeader(http.StatusOK)
}

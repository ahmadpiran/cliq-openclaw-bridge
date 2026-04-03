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

	body, err := io.ReadAll(r.Body)
	if err != nil {
		slog.Error("webhook handler: failed to read body",
			"request_id", requestID,
			"error", err,
		)
		http.Error(w, "could not read body", http.StatusInternalServerError)
		return
	}

	// Zoho Cliq sends automatic link-preview webhooks when a message contains
	// a URL. These payloads contain raw newlines inside JSON string values,
	// making them invalid JSON. Sanitize before attempting to decode.
	sanitized := sanitizeJSON(body)

	slog.Debug("raw payload", "body", string(sanitized))

	var payload ZohoPayload
	if err := json.Unmarshal(sanitized, &payload); err != nil {
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
		Payload:    sanitized,
		ReceivedAt: time.Now(),
	}

	if err := h.pool.Dispatch(job); err != nil {
		slog.Warn("webhook handler: dispatch failed",
			"request_id", requestID,
			"error", err,
		)
		http.Error(w, "service busy, retry later", http.StatusTooManyRequests)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// sanitizeJSON replaces raw control characters inside JSON string values
// that Zoho Cliq embeds in link-preview webhook payloads.
func sanitizeJSON(data []byte) []byte {
	result := make([]byte, 0, len(data))
	inString := false
	escaped := false

	for i := 0; i < len(data); i++ {
		b := data[i]
		if escaped {
			result = append(result, b)
			escaped = false
			continue
		}
		if b == '\\' && inString {
			result = append(result, b)
			escaped = true
			continue
		}
		if b == '"' {
			inString = !inString
			result = append(result, b)
			continue
		}
		if inString {
			switch b {
			case '\n':
				result = append(result, '\\', 'n')
			case '\r':
				result = append(result, '\\', 'r')
			case '\t':
				result = append(result, '\\', 't')
			default:
				result = append(result, b)
			}
			continue
		}
		result = append(result, b)
	}
	return result
}

package handler

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
)

// NotifyHandler receives agent reply pushes from OpenClaw's message:sent hook
// and forwards them to Zoho Cliq via the configured webhook sender.
type NotifyHandler struct {
	secret string
	sender ZohoSender
}

// NewNotifyHandler constructs a NotifyHandler.
// secret is compared against the X-Notify-Secret header in constant time.
func NewNotifyHandler(secret string, sender ZohoSender) *NotifyHandler {
	return &NotifyHandler{secret: secret, sender: sender}
}

type notifyPayload struct {
	Text string `json:"text"`
}

// Handle processes POST /notify from the OpenClaw hook.
func (h *NotifyHandler) Handle(w http.ResponseWriter, r *http.Request) {
	// Validate shared secret — constant time to prevent timing attacks.
	incoming := r.Header.Get("X-Notify-Secret")
	if subtle.ConstantTimeCompare([]byte(incoming), []byte(h.secret)) != 1 {
		slog.Warn("notify: invalid secret", "remote_addr", r.RemoteAddr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1 MiB cap
	if err != nil {
		slog.Error("notify: failed to read body", "error", err)
		http.Error(w, "read error", http.StatusInternalServerError)
		return
	}

	var p notifyPayload
	if err := json.Unmarshal(body, &p); err != nil || p.Text == "" {
		slog.Warn("notify: invalid payload", "error", err)
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	slog.Info("notify: posting agent reply to zoho cliq",
		"reply_len", len(p.Text),
	)

	if err := h.sender.PostToChannel(r.Context(), "", p.Text); err != nil {
		slog.Error("notify: failed to post to zoho cliq", "error", err)
		http.Error(w, "delivery failed", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

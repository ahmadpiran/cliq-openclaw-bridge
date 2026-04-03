package handler

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
)

type NotifyHandler struct {
	secret       string
	sender       ZohoSender
	channel      string // default channel for push-path file delivery
	workspaceDir string
}

func NewNotifyHandler(secret string, sender ZohoSender, channel, workspaceDir string) *NotifyHandler {
	return &NotifyHandler{
		secret:       secret,
		sender:       sender,
		channel:      channel,
		workspaceDir: workspaceDir,
	}
}

type notifyPayload struct {
	Text string `json:"text"`
}

func (h *NotifyHandler) Handle(w http.ResponseWriter, r *http.Request) {
	incoming := r.Header.Get("X-Notify-Secret")
	if subtle.ConstantTimeCompare([]byte(incoming), []byte(h.secret)) != 1 {
		slog.Warn("notify: invalid secret", "remote_addr", r.RemoteAddr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
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

	parsed := parseReply(p.Text, h.workspaceDir)

	if parsed.text != "" {
		slog.Info("notify: posting agent reply to zoho cliq",
			"reply_len", len(parsed.text),
		)
		if err := h.sender.PostToChannel(r.Context(), h.channel, parsed.text); err != nil {
			slog.Error("notify: failed to post to zoho cliq", "error", err)
			http.Error(w, "delivery failed", http.StatusInternalServerError)
			return
		}
	}

	if parsed.filePath != "" {
		slog.Info("notify: sending file to zoho cliq", "file", parsed.filePath)
		if err := h.sender.SendFile(r.Context(), h.channel, parsed.filePath); err != nil {
			slog.Error("notify: failed to send file", "file", parsed.filePath, "error", err)
		}
	}

	w.WriteHeader(http.StatusOK)
}

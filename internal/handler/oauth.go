package handler

import (
	"context"
	"log/slog"
	"net/http"
)

// CodeExchanger is satisfied by *zoho.Refresher.
// Defined here as an interface so the handler is testable without a real Zoho server.
type CodeExchanger interface {
	ExchangeCode(ctx context.Context, code, redirectURI string) error
}

// OAuthHandler handles the one-time Zoho OAuth2 authorization code callback.
// After the user completes the Zoho consent screen, Zoho redirects here with
// a short-lived code that we exchange for a persistent refresh token.
type OAuthHandler struct {
	exchanger   CodeExchanger
	redirectURI string
}

// NewOAuthHandler constructs an OAuthHandler.
func NewOAuthHandler(exchanger CodeExchanger, redirectURI string) *OAuthHandler {
	return &OAuthHandler{exchanger: exchanger, redirectURI: redirectURI}
}

// HandleCallback processes GET /oauth/callback?code=<code>.
//
// Security note: a production deployment should also validate a `state`
// parameter to prevent CSRF. Generate a random state, store it in a
// short-lived server-side session, and reject callbacks where state does
// not match before calling ExchangeCode.
func (h *OAuthHandler) HandleCallback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	if code == "" {
		slog.Warn("oauth callback: missing code parameter",
			"remote_addr", r.RemoteAddr,
		)
		http.Error(w, "missing code parameter", http.StatusBadRequest)
		return
	}

	if err := h.exchanger.ExchangeCode(r.Context(), code, h.redirectURI); err != nil {
		slog.Error("oauth callback: code exchange failed", "error", err)
		http.Error(w, "token exchange failed", http.StatusInternalServerError)
		return
	}

	slog.Info("oauth callback: initial token saved successfully")
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Authorization successful. You may close this window."))
}

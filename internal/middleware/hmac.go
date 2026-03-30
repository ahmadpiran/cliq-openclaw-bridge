package middleware

import (
	"crypto/subtle"
	"io"
	"log/slog"
	"net/http"

	"bytes"
)

const (
	// ZohoSignatureHeader is the header the Deluge script injects on every webhook call.
	ZohoSignatureHeader = "X-Zoho-Webhook-Token"

	// maxBodyBytes caps how much we'll read into memory.
	maxBodyBytes = 2 << 20 // 2 MiB
)

func ZohoHMAC(secret string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := r.Header.Get(ZohoSignatureHeader)
			if token == "" {
				slog.Warn("missing zoho token header",
					"remote_addr", r.RemoteAddr,
					"path", r.URL.Path,
				)
				http.Error(w, "missing token", http.StatusUnauthorized)
				return
			}

			// Constant-time comparison — prevents timing side-channel attacks.
			if !tokenEqual(secret, token) {
				slog.Warn("invalid zoho token",
					"remote_addr", r.RemoteAddr,
					"path", r.URL.Path,
				)
				http.Error(w, "invalid token", http.StatusUnauthorized)
				return
			}

			// Read and restore the body so downstream handlers can read it.
			body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
			if err != nil {
				slog.Error("failed to read request body", "error", err)
				http.Error(w, "could not read body", http.StatusInternalServerError)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))

			next.ServeHTTP(w, r)
		})
	}
}

// tokenEqual compares two strings in constant time.
func tokenEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

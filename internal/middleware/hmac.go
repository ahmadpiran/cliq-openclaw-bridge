package middleware

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

const (
	// ZohoSignatureHeader is the header Zoho Cliq injects on every webhook delivery.
	ZohoSignatureHeader = "X-Zoho-Webhook-Token"

	// maxBodyBytes caps how much we'll read for HMAC validation.
	// Protects against memory exhaustion from a malicious oversized payload.
	// File attachments are streamed separately via the Zoho Files API, not inlined here.
	maxBodyBytes = 2 << 20 // 2 MiB
)

// ZohoHMAC returns a middleware that validates the HMAC-SHA256 signature
// Zoho Cliq attaches to every webhook request.
//
// secret is the shared secret configured in your Zoho Cliq webhook settings.
// Requests with a missing, malformed, or invalid signature are rejected with 401.
func ZohoHMAC(secret string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sig := r.Header.Get(ZohoSignatureHeader)
			if sig == "" {
				slog.Warn("missing zoho signature header",
					"remote_addr", r.RemoteAddr,
					"path", r.URL.Path,
				)
				http.Error(w, "missing signature", http.StatusUnauthorized)
				return
			}

			// Read the body up to the cap, then restore it for downstream handlers.
			body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
			if err != nil {
				slog.Error("failed to read request body for hmac", "error", err)
				http.Error(w, "could not read body", http.StatusInternalServerError)
				return
			}
			// Restore the body so the handler can read it again.
			r.Body = io.NopCloser(bytes.NewReader(body))

			if !validSignature(secret, body, sig) {
				slog.Warn("invalid zoho signature",
					"remote_addr", r.RemoteAddr,
					"path", r.URL.Path,
				)
				http.Error(w, "invalid signature", http.StatusUnauthorized)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// validSignature computes HMAC-SHA256 over body using secret and compares it
// against the provided signature string in constant time.
//
// Zoho sends the digest as a lowercase hex string. We accept both
// "sha256=<hex>" (GitHub-style prefixed) and raw hex to be defensive.
func validSignature(secret string, body []byte, receivedSig string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))

	// Strip optional "sha256=" prefix.
	receivedSig = strings.TrimPrefix(receivedSig, "sha256=")

	// hmac.Equal is constant-time — prevents timing attacks.
	expectedBytes, err := hex.DecodeString(expected)
	if err != nil {
		return false
	}
	receivedBytes, err := hex.DecodeString(receivedSig)
	if err != nil {
		return false
	}

	return hmac.Equal(expectedBytes, receivedBytes)
}

package middleware_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ahmadpiran/cliq-openclaw-bridge/internal/middleware"
)

const testSecret = "supersecret"

// makeSignature is the reference implementation tests use to generate valid sigs.
func makeSignature(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return hex.EncodeToString(mac.Sum(nil))
}

// nextHandler is a sentinel handler that marks whether it was reached.
func nextHandler(reached *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*reached = true
		// Confirm body is still readable downstream.
		b, _ := io.ReadAll(r.Body)
		if string(b) == "" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}

func TestZohoHMAC_ValidSignature(t *testing.T) {
	body := `{"event":"message","text":"hello"}`
	sig := makeSignature(testSecret, body)

	reached := false
	mw := middleware.ZohoHMAC(testSecret)(nextHandler(&reached))

	req := httptest.NewRequest(http.MethodPost, "/webhooks/zoho", bytes.NewBufferString(body))
	req.Header.Set(middleware.ZohoSignatureHeader, sig)
	rec := httptest.NewRecorder()

	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if !reached {
		t.Error("next handler was not called for valid signature")
	}
}

func TestZohoHMAC_ValidSignature_WithPrefix(t *testing.T) {
	body := `{"event":"message","text":"hello"}`
	sig := "sha256=" + makeSignature(testSecret, body)

	reached := false
	mw := middleware.ZohoHMAC(testSecret)(nextHandler(&reached))

	req := httptest.NewRequest(http.MethodPost, "/webhooks/zoho", bytes.NewBufferString(body))
	req.Header.Set(middleware.ZohoSignatureHeader, sig)
	rec := httptest.NewRecorder()

	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 with sha256= prefix, got %d", rec.Code)
	}
}

func TestZohoHMAC_MissingHeader(t *testing.T) {
	reached := false
	mw := middleware.ZohoHMAC(testSecret)(nextHandler(&reached))

	req := httptest.NewRequest(http.MethodPost, "/webhooks/zoho", bytes.NewBufferString("body"))
	rec := httptest.NewRecorder()

	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
	if reached {
		t.Error("next handler must not be called when header is missing")
	}
}

func TestZohoHMAC_WrongSignature(t *testing.T) {
	reached := false
	mw := middleware.ZohoHMAC(testSecret)(nextHandler(&reached))

	req := httptest.NewRequest(http.MethodPost, "/webhooks/zoho", bytes.NewBufferString("body"))
	req.Header.Set(middleware.ZohoSignatureHeader, "deaadbeefdeadbeef")
	rec := httptest.NewRecorder()

	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
	if reached {
		t.Error("next handler must not be called on invalid signature")
	}
}

func TestZohoHMAC_BodyStillReadableDownstream(t *testing.T) {
	body := `{"event":"message"}`
	sig := makeSignature(testSecret, body)

	reached := false
	mw := middleware.ZohoHMAC(testSecret)(nextHandler(&reached))

	req := httptest.NewRequest(http.MethodPost, "/webhooks/zoho", bytes.NewBufferString(body))
	req.Header.Set(middleware.ZohoSignatureHeader, sig)
	rec := httptest.NewRecorder()

	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("body was not restored for downstream handler, got %d", rec.Code)
	}
}

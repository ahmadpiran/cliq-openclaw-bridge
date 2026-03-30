package middleware_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ahmadpiran/cliq-openclaw-bridge/internal/middleware"
)

const testSecret = "supersecret"

func nextHandler(reached *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*reached = true
		b, _ := io.ReadAll(r.Body)
		if len(b) == 0 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}

func TestZohoHMAC_ValidToken(t *testing.T) {
	reached := false
	mw := middleware.ZohoHMAC(testSecret)(nextHandler(&reached))

	req := httptest.NewRequest(http.MethodPost, "/webhooks/zoho",
		bytes.NewBufferString(`{"type":"message"}`))
	req.Header.Set(middleware.ZohoSignatureHeader, testSecret)
	rec := httptest.NewRecorder()

	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if !reached {
		t.Error("next handler was not called for valid token")
	}
}

func TestZohoHMAC_MissingHeader(t *testing.T) {
	reached := false
	mw := middleware.ZohoHMAC(testSecret)(nextHandler(&reached))

	req := httptest.NewRequest(http.MethodPost, "/webhooks/zoho",
		bytes.NewBufferString("body"))
	rec := httptest.NewRecorder()

	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
	if reached {
		t.Error("next handler must not be called when header is missing")
	}
}

func TestZohoHMAC_WrongToken(t *testing.T) {
	reached := false
	mw := middleware.ZohoHMAC(testSecret)(nextHandler(&reached))

	req := httptest.NewRequest(http.MethodPost, "/webhooks/zoho",
		bytes.NewBufferString("body"))
	req.Header.Set(middleware.ZohoSignatureHeader, "wrongsecret")
	rec := httptest.NewRecorder()

	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
	if reached {
		t.Error("next handler must not be called on invalid token")
	}
}

func TestZohoHMAC_BodyStillReadableDownstream(t *testing.T) {
	reached := false
	mw := middleware.ZohoHMAC(testSecret)(nextHandler(&reached))

	req := httptest.NewRequest(http.MethodPost, "/webhooks/zoho",
		bytes.NewBufferString(`{"event":"message"}`))
	req.Header.Set(middleware.ZohoSignatureHeader, testSecret)
	rec := httptest.NewRecorder()

	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("body was not restored for downstream handler, got %d", rec.Code)
	}
}

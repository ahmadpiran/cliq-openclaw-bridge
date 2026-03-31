package handler_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ahmadpiran/cliq-openclaw-bridge/internal/handler"
)

func TestNotifyHandler_ValidSecret(t *testing.T) {
	sender := &fakeZohoSender{}
	h := handler.NewNotifyHandler("test-secret", sender)

	req := httptest.NewRequest(http.MethodPost, "/notify",
		bytes.NewBufferString(`{"text":"Hello from agent"}`))
	req.Header.Set("X-Notify-Secret", "test-secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.Handle(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if sender.sent.Load() != 1 {
		t.Error("expected sender to be called once")
	}
	if sender.lastText != "Hello from agent" {
		t.Errorf("wrong text: %s", sender.lastText)
	}
}

func TestNotifyHandler_InvalidSecret(t *testing.T) {
	sender := &fakeZohoSender{}
	h := handler.NewNotifyHandler("test-secret", sender)

	req := httptest.NewRequest(http.MethodPost, "/notify",
		bytes.NewBufferString(`{"text":"Hello"}`))
	req.Header.Set("X-Notify-Secret", "wrong-secret")
	rec := httptest.NewRecorder()

	h.Handle(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
	if sender.sent.Load() != 0 {
		t.Error("sender must not be called with invalid secret")
	}
}

func TestNotifyHandler_EmptyText(t *testing.T) {
	sender := &fakeZohoSender{}
	h := handler.NewNotifyHandler("test-secret", sender)

	req := httptest.NewRequest(http.MethodPost, "/notify",
		bytes.NewBufferString(`{"text":""}`))
	req.Header.Set("X-Notify-Secret", "test-secret")
	rec := httptest.NewRecorder()

	h.Handle(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
	if sender.sent.Load() != 0 {
		t.Error("sender must not be called for empty text")
	}
}

func TestNotifyHandler_MissingSecret(t *testing.T) {
	sender := &fakeZohoSender{}
	h := handler.NewNotifyHandler("test-secret", sender)

	req := httptest.NewRequest(http.MethodPost, "/notify",
		bytes.NewBufferString(`{"text":"Hello"}`))
	// No X-Notify-Secret header
	rec := httptest.NewRecorder()

	h.Handle(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

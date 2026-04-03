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
	h := handler.NewNotifyHandler("test-secret", sender, "CT_123", "")

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
	h := handler.NewNotifyHandler("test-secret", sender, "CT_123", "")

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
	h := handler.NewNotifyHandler("test-secret", sender, "CT_123", "")

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
	h := handler.NewNotifyHandler("test-secret", sender, "CT_123", "")

	req := httptest.NewRequest(http.MethodPost, "/notify",
		bytes.NewBufferString(`{"text":"Hello"}`))
	rec := httptest.NewRecorder()

	h.Handle(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

func TestNotifyHandler_FileTagTriggersFileSend(t *testing.T) {
	sender := &fakeZohoSender{}
	h := handler.NewNotifyHandler("test-secret", sender, "CT_123", "/data/workspace")

	req := httptest.NewRequest(http.MethodPost, "/notify",
		bytes.NewBufferString(`{"text":"Done!\n[FILE:~/workspace/uploads/report.pdf]"}`))
	req.Header.Set("X-Notify-Secret", "test-secret")
	rec := httptest.NewRecorder()

	h.Handle(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if sender.sent.Load() != 1 {
		t.Errorf("expected 1 text post, got %d", sender.sent.Load())
	}
	if sender.filesSent.Load() != 1 {
		t.Errorf("expected 1 file send, got %d", sender.filesSent.Load())
	}
	if sender.lastFilePath != "/data/workspace/uploads/report.pdf" {
		t.Errorf("wrong file path: %s", sender.lastFilePath)
	}
}

func TestNotifyHandler_FileOnlyTagNoText(t *testing.T) {
	sender := &fakeZohoSender{}
	h := handler.NewNotifyHandler("test-secret", sender, "CT_123", "/data/workspace")

	// Text field must be non-empty (the tag counts), but after stripping there's no text.
	req := httptest.NewRequest(http.MethodPost, "/notify",
		bytes.NewBufferString(`{"text":"[FILE:~/workspace/uploads/data.csv]"}`))
	req.Header.Set("X-Notify-Secret", "test-secret")
	rec := httptest.NewRecorder()

	h.Handle(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	// No text to post, only a file.
	if sender.sent.Load() != 0 {
		t.Errorf("expected 0 text posts, got %d", sender.sent.Load())
	}
	if sender.filesSent.Load() != 1 {
		t.Errorf("expected 1 file send, got %d", sender.filesSent.Load())
	}
}

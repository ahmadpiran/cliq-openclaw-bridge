package handler_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ahmadpiran/cliq-openclaw-bridge/internal/handler"
)

// fakeExchanger is a test double for CodeExchanger.
type fakeExchanger struct {
	err error
}

func (f *fakeExchanger) ExchangeCode(_ context.Context, _, _ string) error {
	return f.err
}

func TestHandleCallback_MissingCode(t *testing.T) {
	h := handler.NewOAuthHandler(&fakeExchanger{}, "https://example.com/callback")

	req := httptest.NewRequest(http.MethodGet, "/oauth/callback", nil)
	rec := httptest.NewRecorder()

	h.HandleCallback(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

func TestHandleCallback_ExchangeSuccess(t *testing.T) {
	h := handler.NewOAuthHandler(&fakeExchanger{err: nil}, "https://example.com/callback")

	req := httptest.NewRequest(http.MethodGet, "/oauth/callback?code=valid-code", nil)
	rec := httptest.NewRecorder()

	h.HandleCallback(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestHandleCallback_ExchangeFailure(t *testing.T) {
	h := handler.NewOAuthHandler(
		&fakeExchanger{err: errors.New("zoho refused")},
		"https://example.com/callback",
	)

	req := httptest.NewRequest(http.MethodGet, "/oauth/callback?code=bad-code", nil)
	rec := httptest.NewRecorder()

	h.HandleCallback(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", rec.Code)
	}
}

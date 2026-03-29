package zoho_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ahmadpiran/cliq-openclaw-bridge/internal/store"
	"github.com/ahmadpiran/cliq-openclaw-bridge/internal/zoho"
)

// newTestStore creates a temporary BoltDB store for the duration of a test.
func newTestStore(t *testing.T) *store.TokenStore {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "bolt-test-*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	ts, err := store.NewTokenStore(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ts.Close() })
	return ts
}

// newTestRefresher wires a Refresher to a fake Zoho token server and a temp store.
// The returned server must be closed by the caller (registered via t.Cleanup).
func newTestRefresher(t *testing.T, handler http.HandlerFunc) (*zoho.Refresher, *store.TokenStore, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	ts := newTestStore(t)
	cfg := zoho.RefresherConfig{
		ClientID:     "test-client-id",
		ClientSecret: "test-client-secret",
		TokenKey:     "zoho:test",
		// Override the token URL so tests hit the fake server.
		TokenURL: srv.URL,
	}
	return zoho.NewRefresher(cfg, ts), ts, srv
}

func TestValidToken_ReturnsCachedTokenWhenFresh(t *testing.T) {
	var calls atomic.Int32
	r, ts, _ := newTestRefresher(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	})

	// Pre-populate a fresh token — should be returned without a network call.
	_ = ts.Save("zoho:test", store.Token{
		AccessToken:  "fresh-token",
		RefreshToken: "refresh-token",
		ExpiresAt:    time.Now().Add(1 * time.Hour),
	})

	got, err := r.ValidToken(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "fresh-token" {
		t.Errorf("expected fresh-token, got %s", got)
	}
	if calls.Load() != 0 {
		t.Error("expected zero network calls for a fresh token")
	}
}

func TestValidToken_RefreshesExpiredToken(t *testing.T) {
	var calls atomic.Int32
	r, ts, _ := newTestRefresher(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "new-access-token",
			"expires_in":   3600,
			"token_type":   "Bearer",
		})
	})

	// Pre-populate an expired token.
	_ = ts.Save("zoho:test", store.Token{
		AccessToken:  "old-token",
		RefreshToken: "refresh-token",
		ExpiresAt:    time.Now().Add(-1 * time.Minute),
	})

	got, err := r.ValidToken(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "new-access-token" {
		t.Errorf("expected new-access-token, got %s", got)
	}
	if calls.Load() != 1 {
		t.Errorf("expected exactly 1 refresh call, got %d", calls.Load())
	}

	// Confirm new token was persisted.
	saved, err := ts.Load("zoho:test")
	if err != nil {
		t.Fatalf("load after refresh: %v", err)
	}
	if saved.AccessToken != "new-access-token" {
		t.Errorf("persisted token mismatch: %s", saved.AccessToken)
	}
}

func TestValidToken_ErrorWhenNoRefreshToken(t *testing.T) {
	r, _, _ := newTestRefresher(t, func(w http.ResponseWriter, _ *http.Request) {
		// Should never be called.
		w.WriteHeader(http.StatusOK)
	})

	// No token in store at all.
	_, err := r.ValidToken(context.Background())
	if err == nil {
		t.Fatal("expected error when no token is stored, got nil")
	}
}

func TestValidToken_ConcurrentRefreshOnlyCallsZohoOnce(t *testing.T) {
	var calls atomic.Int32

	// Add a small delay so concurrent goroutines pile up at the lock.
	r, ts, _ := newTestRefresher(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "concurrent-token",
			"expires_in":   3600,
		})
	})

	_ = ts.Save("zoho:test", store.Token{
		AccessToken:  "expired",
		RefreshToken: "refresh-token",
		ExpiresAt:    time.Now().Add(-1 * time.Minute),
	})

	const goroutines = 20
	var wg sync.WaitGroup
	tokens := make([]string, goroutines)
	errs := make([]error, goroutines)

	for i := range goroutines {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			tokens[idx], errs[idx] = r.ValidToken(context.Background())
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d error: %v", i, err)
		}
		if tokens[i] != "concurrent-token" {
			t.Errorf("goroutine %d got wrong token: %s", i, tokens[i])
		}
	}

	if n := calls.Load(); n != 1 {
		t.Errorf("expected exactly 1 Zoho API call under concurrent load, got %d", n)
	}
}

func TestValidToken_ZohoErrorResponse(t *testing.T) {
	r, ts, _ := newTestRefresher(t, func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"error": "invalid_client",
		})
	})

	_ = ts.Save("zoho:test", store.Token{
		AccessToken:  "expired",
		RefreshToken: "bad-refresh",
		ExpiresAt:    time.Now().Add(-1 * time.Minute),
	})

	_, err := r.ValidToken(context.Background())
	if err == nil {
		t.Fatal("expected error for Zoho error response, got nil")
	}
}

func TestExchangeCode_Success(t *testing.T) {
	var calls atomic.Int32
	r, ts, _ := newTestRefresher(t, func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		// Confirm it's a code exchange, not a refresh.
		if err := req.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		if req.FormValue("grant_type") != "authorization_code" {
			t.Errorf("unexpected grant_type: %s", req.FormValue("grant_type"))
		}
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "initial-access",
			"refresh_token": "initial-refresh",
			"expires_in":    3600,
		})
	})

	if err := r.ExchangeCode(context.Background(), "auth-code-abc", "https://example.com/callback"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	saved, err := ts.Load("zoho:test")
	if err != nil {
		t.Fatalf("load after exchange: %v", err)
	}
	if saved.AccessToken != "initial-access" || saved.RefreshToken != "initial-refresh" {
		t.Errorf("unexpected saved token: %+v", saved)
	}
	if calls.Load() != 1 {
		t.Errorf("expected 1 call, got %d", calls.Load())
	}
}

func TestExchangeCode_ZohoReturnsError(t *testing.T) {
	r, _, _ := newTestRefresher(t, func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"error": "invalid_code",
		})
	})

	err := r.ExchangeCode(context.Background(), "bad-code", "https://example.com/callback")
	if err == nil {
		t.Fatal("expected error for Zoho error response, got nil")
	}
}

func TestExchangeCode_MissingTokensInResponse(t *testing.T) {
	r, _, _ := newTestRefresher(t, func(w http.ResponseWriter, _ *http.Request) {
		// Simulates Zoho returning a partial response.
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "only-access",
			// refresh_token deliberately missing
			"expires_in": 3600,
		})
	})

	err := r.ExchangeCode(context.Background(), "code-xyz", "https://example.com/callback")
	if err == nil {
		t.Fatal("expected error when refresh_token is absent, got nil")
	}
}

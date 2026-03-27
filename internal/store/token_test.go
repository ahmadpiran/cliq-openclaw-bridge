package store_test

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/ahmadpiran/cliq-openclaw-bridge/internal/store"
)

func TestTokenStore_RoundTrip(t *testing.T) {
	// Use a temp file so tests never pollute the dev database.
	f, err := os.CreateTemp(t.TempDir(), "test-bolt-*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	s, err := store.NewTokenStore(f.Name())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	const key = "zoho:default"

	// Load from empty store → must return ErrTokenNotFound.
	_, err = s.Load(key)
	if !errors.Is(err, store.ErrTokenNotFound) {
		t.Fatalf("expected ErrTokenNotFound, got %v", err)
	}

	// Save a token and reload it.
	want := store.Token{
		AccessToken:  "acc_abc123",
		RefreshToken: "ref_xyz789",
		ExpiresAt:    time.Now().Add(1 * time.Hour).Truncate(time.Second),
	}
	if err := s.Save(key, want); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := s.Load(key)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.AccessToken != want.AccessToken || got.RefreshToken != want.RefreshToken {
		t.Errorf("token mismatch: got %+v, want %+v", got, want)
	}

	// IsExpired — a future token must not be expired.
	if got.IsExpired() {
		t.Error("fresh token should not be expired")
	}

	// Delete and confirm eviction.
	if err := s.Delete(key); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, err = s.Load(key)
	if !errors.Is(err, store.ErrTokenNotFound) {
		t.Fatal("expected ErrTokenNotFound after delete")
	}
}

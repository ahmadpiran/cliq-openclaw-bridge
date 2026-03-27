package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
)

// ErrTokenNotFound is returned when no token exists for a given key.
// Callers can check for this specifically to trigger an initial OAuth flow.
var ErrTokenNotFound = errors.New("token not found")

// bucket is the BoltDB bucket that holds all token records.
// A single bucket is sufficient — keys are namespaced by the token key string.
var bucket = []byte("oauth_tokens")

// Token holds a Zoho OAuth token pair and its expiry metadata.
// This is what gets serialized to disk.
type Token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// IsExpired reports whether the access token needs refreshing.
// The 30-second buffer avoids using a token that expires mid-request.
func (t *Token) IsExpired() bool {
	return time.Now().After(t.ExpiresAt.Add(-30 * time.Second))
}

// TokenStore wraps a BoltDB instance and exposes a minimal token CRUD interface.
// All methods are safe for concurrent use — BoltDB serializes writes internally.
type TokenStore struct {
	db *bolt.DB
}

// NewTokenStore opens (or creates) the BoltDB file at the given path and ensures
// the token bucket exists. The caller must call Close() when done.
func NewTokenStore(path string) (*TokenStore, error) {
	db, err := bolt.Open(path, 0600, &bolt.Options{
		Timeout: 2 * time.Second, // Fail fast if another process holds the file lock.
	})
	if err != nil {
		return nil, fmt.Errorf("open bolt db: %w", err)
	}

	// CreateBucketIfNotExists must run inside an Update transaction.
	if err := db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(bucket)
		return err
	}); err != nil {
		db.Close()
		return nil, fmt.Errorf("create token bucket: %w", err)
	}

	return &TokenStore{db: db}, nil
}

// Save persists a Token under the given key, overwriting any existing value.
// key should be a stable identifier, e.g. "zoho:default" or a per-user ID.
func (s *TokenStore) Save(key string, t Token) error {
	data, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("marshal token: %w", err)
	}

	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		if err := b.Put([]byte(key), data); err != nil {
			return fmt.Errorf("bolt put: %w", err)
		}
		return nil
	})
}

// Load retrieves the Token stored under key.
// Returns ErrTokenNotFound if the key does not exist.
func (s *TokenStore) Load(key string) (Token, error) {
	var t Token

	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		data := b.Get([]byte(key))
		if data == nil {
			return ErrTokenNotFound
		}
		return json.Unmarshal(data, &t)
	})
	if err != nil {
		return Token{}, err // ErrTokenNotFound passes through unwrapped — callers use errors.Is()
	}

	return t, nil
}

// Delete removes the token for the given key.
// It is not an error to delete a key that does not exist.
func (s *TokenStore) Delete(key string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucket).Delete([]byte(key))
	})
}

// Close releases the BoltDB file lock. Must be called on shutdown.
func (s *TokenStore) Close() error {
	return s.db.Close()
}

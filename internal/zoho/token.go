package zoho

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ahmadpiran/cliq-openclaw-bridge/internal/store"
)

const (
	// zohoTokenURL is the Zoho OAuth2 token endpoint.
	zohoTokenURL = "https://accounts.zoho.com/oauth/v2/token"

	// refreshTimeout is the per-attempt deadline for a token refresh call.
	refreshTimeout = 10 * time.Second
)

// zohoTokenResponse maps the JSON body Zoho returns on a successful refresh.
type zohoTokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"` // seconds
	TokenType   string `json:"token_type"`
	Error       string `json:"error"` // non-empty on failure
}

// RefresherConfig holds the credentials needed to refresh a Zoho OAuth token.
type RefresherConfig struct {
	ClientID     string
	ClientSecret string
	TokenKey     string

	// TokenURL overrides the default Zoho token endpoint.
	// Leave empty in production; set in tests to point at a fake server.
	TokenURL string
}

// Refresher manages the Zoho OAuth access token lifecycle.
// It is safe for concurrent use — a mutex ensures only one refresh
// runs at a time even when multiple workers detect expiry simultaneously.
type Refresher struct {
	cfg   RefresherConfig
	store *store.TokenStore
	http  *http.Client
	mu    sync.Mutex
}

// NewRefresher constructs a Refresher wired to the given token store.
func NewRefresher(cfg RefresherConfig, ts *store.TokenStore) *Refresher {
	return &Refresher{
		cfg:   cfg,
		store: ts,
		http:  &http.Client{Timeout: refreshTimeout},
	}
}

// ValidToken returns a valid access token, refreshing it first if necessary.
// This is the only method the rest of the codebase should call.
//
// Concurrency model: if two workers simultaneously observe an expired token,
// the mutex ensures only one refresh call is made to Zoho. The second worker
// waits, then reads the token the first worker already refreshed.
func (r *Refresher) ValidToken(ctx context.Context) (string, error) {
	// Optimistic read — no lock needed for a valid non-expired token.
	t, err := r.store.Load(r.cfg.TokenKey)
	if err == nil && !t.IsExpired() {
		return t.AccessToken, nil
	}

	// Token is missing or expired — acquire the lock and refresh.
	r.mu.Lock()
	defer r.mu.Unlock()

	// Re-check after acquiring the lock: another goroutine may have
	// already refreshed while we were waiting.
	t, err = r.store.Load(r.cfg.TokenKey)
	if err == nil && !t.IsExpired() {
		slog.Debug("token already refreshed by another worker")
		return t.AccessToken, nil
	}

	slog.Info("zoho access token expired or missing, refreshing",
		"token_key", r.cfg.TokenKey,
	)

	newToken, err := r.refresh(ctx, t.RefreshToken)
	if err != nil {
		return "", fmt.Errorf("token refresh: %w", err)
	}

	if err := r.store.Save(r.cfg.TokenKey, newToken); err != nil {
		return "", fmt.Errorf("persist refreshed token: %w", err)
	}

	slog.Info("zoho token refreshed and persisted",
		"token_key", r.cfg.TokenKey,
		"expires_at", newToken.ExpiresAt,
	)

	return newToken.AccessToken, nil
}

// SaveInitialToken persists the first token obtained during the initial
// OAuth authorization flow. Call this once after the user completes the
// Zoho OAuth consent screen and you have a refresh token in hand.
func (r *Refresher) SaveInitialToken(accessToken, refreshToken string, expiresIn int) error {
	t := store.Token{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(expiresIn) * time.Second),
	}
	return r.store.Save(r.cfg.TokenKey, t)
}

// refresh exchanges the refresh token for a new access token via Zoho's
// OAuth2 token endpoint and returns the resulting store.Token.
func (r *Refresher) refresh(ctx context.Context, refreshToken string) (store.Token, error) {
	if refreshToken == "" {
		return store.Token{}, fmt.Errorf(
			"no refresh token stored under key %q — run the initial OAuth flow first",
			r.cfg.TokenKey,
		)
	}

	body := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {r.cfg.ClientID},
		"client_secret": {r.cfg.ClientSecret},
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		r.tokenEndpoint(),
		strings.NewReader(body.Encode()),
	)
	if err != nil {
		return store.Token{}, fmt.Errorf("build refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := r.http.Do(req)
	if err != nil {
		return store.Token{}, fmt.Errorf("refresh request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return store.Token{}, fmt.Errorf("read refresh response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return store.Token{}, fmt.Errorf("refresh response status %d: %s",
			resp.StatusCode, string(raw))
	}

	var tr zohoTokenResponse
	if err := json.Unmarshal(raw, &tr); err != nil {
		return store.Token{}, fmt.Errorf("decode refresh response: %w", err)
	}
	if tr.Error != "" {
		return store.Token{}, fmt.Errorf("zoho token error: %s", tr.Error)
	}
	if tr.AccessToken == "" {
		return store.Token{}, fmt.Errorf("zoho returned empty access token")
	}

	return store.Token{
		AccessToken:  tr.AccessToken,
		RefreshToken: refreshToken, // Zoho reuses the same refresh token.
		ExpiresAt:    time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second),
	}, nil
}

// tokenEndpoint returns the effective token URL, falling back to the Zoho default.
func (r *Refresher) tokenEndpoint() string {
	if r.cfg.TokenURL != "" {
		return r.cfg.TokenURL
	}
	return zohoTokenURL
}

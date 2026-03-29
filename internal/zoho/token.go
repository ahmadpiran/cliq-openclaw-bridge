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
	zohoTokenURL   = "https://accounts.zoho.com/oauth/v2/token"
	refreshTimeout = 10 * time.Second
)

// zohoTokenResponse maps the JSON Zoho returns on both code exchange and refresh.
type zohoTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"` // Present on code exchange; empty on refresh.
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
	Error        string `json:"error"`
}

// RefresherConfig holds the credentials needed to manage a Zoho OAuth token.
type RefresherConfig struct {
	ClientID     string
	ClientSecret string
	TokenKey     string

	// TokenURL overrides the Zoho token endpoint. Leave empty in production;
	// set in tests to point at a fake server.
	TokenURL string
}

// Refresher manages the Zoho OAuth access token lifecycle.
// It is safe for concurrent use.
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

// ValidToken returns a valid Zoho access token, refreshing transparently if expired.
func (r *Refresher) ValidToken(ctx context.Context) (string, error) {
	// Optimistic read — no lock needed when the token is fresh.
	t, err := r.store.Load(r.cfg.TokenKey)
	if err == nil && !t.IsExpired() {
		return t.AccessToken, nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Re-check after lock acquisition — another goroutine may have already refreshed.
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

// ExchangeCode trades a Zoho authorization code for an access + refresh token pair
// and persists it. Call this once after the user completes the OAuth consent screen.
func (r *Refresher) ExchangeCode(ctx context.Context, code, redirectURI string) error {
	body := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {r.cfg.ClientID},
		"client_secret": {r.cfg.ClientSecret},
		"redirect_uri":  {redirectURI},
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		r.tokenEndpoint(),
		strings.NewReader(body.Encode()),
	)
	if err != nil {
		return fmt.Errorf("build code exchange request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := r.http.Do(req)
	if err != nil {
		return fmt.Errorf("code exchange request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return fmt.Errorf("read code exchange response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("code exchange status %d: %s", resp.StatusCode, string(raw))
	}

	var tr zohoTokenResponse
	if err := json.Unmarshal(raw, &tr); err != nil {
		return fmt.Errorf("decode code exchange response: %w", err)
	}
	if tr.Error != "" {
		return fmt.Errorf("zoho code exchange error: %s", tr.Error)
	}
	if tr.AccessToken == "" || tr.RefreshToken == "" {
		return fmt.Errorf("zoho code exchange: incomplete token response")
	}

	return r.SaveInitialToken(tr.AccessToken, tr.RefreshToken, tr.ExpiresIn)
}

// SaveInitialToken persists the first token pair obtained after OAuth consent.
func (r *Refresher) SaveInitialToken(accessToken, refreshToken string, expiresIn int) error {
	t := store.Token{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(expiresIn) * time.Second),
	}
	return r.store.Save(r.cfg.TokenKey, t)
}

// refresh exchanges a refresh token for a new access token.
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

// tokenEndpoint returns the effective token URL.
func (r *Refresher) tokenEndpoint() string {
	if r.cfg.TokenURL != "" {
		return r.cfg.TokenURL
	}
	return zohoTokenURL
}

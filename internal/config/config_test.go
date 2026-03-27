package config_test

import (
	"testing"
	"time"

	"github.com/ahmadpiran/cliq-openclaw-bridge/internal/config"
)

// setEnv sets multiple env vars for the duration of a test and restores them
// on cleanup — safe to use in parallel subtests.
func setEnv(t *testing.T, pairs map[string]string) {
	t.Helper()
	for k, v := range pairs {
		t.Setenv(k, v)
	}
}

// minimalEnv is the smallest set of env vars that satisfies Load().
func minimalEnv(t *testing.T) {
	t.Helper()
	setEnv(t, map[string]string{
		"ZOHO_WEBHOOK_SECRET": "secret",
		"ZOHO_CLIENT_ID":      "client-id",
		"ZOHO_CLIENT_SECRET":  "client-secret",
		"OPENCLAW_BASE_URL":   "https://api.openclaw.io/v1",
		"OPENCLAW_API_KEY":    "oc-key",
	})
}

func TestLoad_MinimalValidConfig(t *testing.T) {
	minimalEnv(t)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if cfg.Zoho.WebhookSecret != "secret" {
		t.Errorf("unexpected WebhookSecret: %s", cfg.Zoho.WebhookSecret)
	}
	if cfg.Server.Addr() != ":8080" {
		t.Errorf("unexpected default addr: %s", cfg.Server.Addr())
	}
	if cfg.Zoho.TokenKey != "zoho:default" {
		t.Errorf("unexpected default token key: %s", cfg.Zoho.TokenKey)
	}
	if cfg.Worker.Workers != 8 {
		t.Errorf("unexpected default worker count: %d", cfg.Worker.Workers)
	}
}

func TestLoad_MissingRequiredVars(t *testing.T) {
	// Deliberately set NO env vars — Load() must return an aggregated error.
	cfg, err := config.Load()
	if err == nil {
		t.Fatal("expected error for missing required vars, got nil")
	}
	if cfg != nil {
		t.Error("expected nil config when validation fails")
	}

	// Confirm all required keys are mentioned in the error message.
	for _, key := range []string{
		"ZOHO_WEBHOOK_SECRET",
		"ZOHO_CLIENT_ID",
		"ZOHO_CLIENT_SECRET",
		"OPENCLAW_BASE_URL",
		"OPENCLAW_API_KEY",
	} {
		if !contains(err.Error(), key) {
			t.Errorf("error message missing mention of %s:\n%s", key, err.Error())
		}
	}
}

func TestLoad_OverridesAndCustomValues(t *testing.T) {
	minimalEnv(t)
	setEnv(t, map[string]string{
		"PORT":                     "9090",
		"WORKER_COUNT":             "16",
		"WORKER_QUEUE_DEPTH":       "1024",
		"WORKER_JOB_TIMEOUT":       "1m",
		"OPENCLAW_MAX_RETRIES":     "6",
		"OPENCLAW_INITIAL_BACKOFF": "200ms",
		"SERVER_SHUTDOWN_TIMEOUT":  "45s",
	})

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Server.Addr() != ":9090" {
		t.Errorf("expected :9090, got %s", cfg.Server.Addr())
	}
	if cfg.Worker.Workers != 16 {
		t.Errorf("expected 16 workers, got %d", cfg.Worker.Workers)
	}
	if cfg.Worker.QueueDepth != 1024 {
		t.Errorf("expected queue depth 1024, got %d", cfg.Worker.QueueDepth)
	}
	if cfg.Worker.JobTimeout != time.Minute {
		t.Errorf("expected 1m job timeout, got %s", cfg.Worker.JobTimeout)
	}
	if cfg.OpenClaw.MaxRetries != 6 {
		t.Errorf("expected 6 retries, got %d", cfg.OpenClaw.MaxRetries)
	}
	if cfg.OpenClaw.InitialBackoff != 200*time.Millisecond {
		t.Errorf("expected 200ms backoff, got %s", cfg.OpenClaw.InitialBackoff)
	}
	if cfg.Server.ShutdownTimeout != 45*time.Second {
		t.Errorf("expected 45s shutdown, got %s", cfg.Server.ShutdownTimeout)
	}
}

func TestLoad_InvalidIntFallsBackToDefault(t *testing.T) {
	minimalEnv(t)
	t.Setenv("WORKER_COUNT", "not-a-number")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Worker.Workers != 8 {
		t.Errorf("expected default 8 workers on bad input, got %d", cfg.Worker.Workers)
	}
}

func TestLoad_InvalidDurationFallsBackToDefault(t *testing.T) {
	minimalEnv(t)
	t.Setenv("WORKER_JOB_TIMEOUT", "forever")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Worker.JobTimeout != 30*time.Second {
		t.Errorf("expected default 30s job timeout on bad input, got %s", cfg.Worker.JobTimeout)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr ||
		len(s) > 0 && containsRune(s, substr))
}

func containsRune(s, substr string) bool {
	for i := range s {
		if i+len(substr) <= len(s) && s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
